package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestChatRotationReusesConversationRequestID 轮转失败（全部账号 402）场景：
// 所有出站请求（3 号各打一次）的 X-Conversation-Request-ID 必须相同（后台聚合
// 主键），X-Conversation-ID 透传客户端原值，X-Root-Request-ID 跟随
// conversationRequestID，消息级 ID 每次出站不同，链路族合法。
func TestChatRotationReusesConversationRequestID(t *testing.T) {
	var reqs []http.Header
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reqs = append(reqs, r.Header.Clone())
			// 全部 402 余额不足 → 每号冷却换号，轮转后 503。
			return &http.Response{
				StatusCode: 402,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":1,"msg":"余额不足"}`)),
			}, nil
		})},
		ChatHTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reqs = append(reqs, r.Header.Clone())
			return &http.Response{
				StatusCode: 402,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":1,"msg":"余额不足"}`)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at-a3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s (want 503 all fail)", rec.Code, rec.Body)
	}
	if len(reqs) < 3 {
		t.Fatalf("upstream calls=%d want >=3 (rotation)", len(reqs))
	}
	// 聚合主键：所有出站全相同。
	first := reqs[0].Get("X-Conversation-Request-ID")
	if first == "" {
		t.Fatal("X-Conversation-Request-ID missing on outbound")
	}
	for i, hdr := range reqs[1:] {
		if got := hdr.Get("X-Conversation-Request-ID"); got != first {
			t.Errorf("outbound %d X-Conversation-Request-ID=%q want %q (reuse across rotation)", i+2, got, first)
		}
	}
	// 对话透传。
	for i, hdr := range reqs {
		if got := hdr.Get("X-Conversation-ID"); got != "conv-1" {
			t.Errorf("outbound %d X-Conversation-ID=%q want conv-1", i+1, got)
		}
	}
	// Root = conversationRequestID。
	for i, hdr := range reqs {
		if got := hdr.Get("X-Root-Request-ID"); got != first {
			t.Errorf("outbound %d X-Root-Request-ID=%q want %q", i+1, got, first)
		}
	}
	// 消息级 ID（每条消息独立）：各出站不同（每条 ChatStream 独立），格式合法且
	// X-Conversation-Message-ID == X-Request-ID。
	msgIDs := map[string]bool{}
	for i, hdr := range reqs {
		mid := hdr.Get("X-Conversation-Message-ID")
		if mid == "" || mid != hdr.Get("X-Request-ID") {
			t.Errorf("outbound %d message id mismatch: msg=%q request=%q", i+1, mid, hdr.Get("X-Request-ID"))
		}
		msgIDs[mid] = true
	}
	if len(msgIDs) != len(reqs) {
		t.Errorf("each outbound message should have distinct message id, got %d unique for %d calls", len(msgIDs), len(reqs))
	}
	// 链路族始终合法。
	for i, hdr := range reqs {
		if trace := hdr.Get("X-B3-TraceId"); !isValidB3Trace(trace) {
			t.Errorf("outbound %d X-B3-TraceId=%q not 16/32 hex", i+1, trace)
		}
		if span := hdr.Get("X-B3-SpanId"); len(span) != 16 || !isValidB3Trace(span) {
			t.Errorf("outbound %d X-B3-SpanId=%q not 16 hex", i+1, span)
		}
		if got := hdr.Get("X-B3-Sampled"); got != "1" {
			t.Errorf("outbound %d X-B3-Sampled=%q want 1", i+1, got)
		}
	}
}

// TestChatConversationIDPassthrough 成功路径：body 带 camelCase conversationId →
// 出站 X-Conversation-ID 原样透传；conversationRequestID 缺入站头时按会话 key 稳定生成。
func TestChatConversationIDPassthrough(t *testing.T) {
	var captured http.Header
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			captured = r.Header.Clone()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[],"conversationId":"conv-camel"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := captured.Get("X-Conversation-ID"); got != "conv-camel" {
		t.Errorf("X-Conversation-ID=%q want conv-camel (camelCase passthrough)", got)
	}
	if got := captured.Get("X-Conversation-Request-ID"); got == "" {
		t.Error("X-Conversation-Request-ID missing")
	}
}

// TestChatConversationRequestIDInboundPassthrough 入站自带 X-Conversation-Request-ID →
// 出站原样透传（客户端已有自己的对话轮 ID 时以客户端为准）。
func TestChatConversationRequestIDInboundPassthrough(t *testing.T) {
	var captured http.Header
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			captured = r.Header.Clone()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("X-Conversation-Request-ID", "client-req-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := captured.Get("X-Conversation-Request-ID"); got != "client-req-1" {
		t.Errorf("X-Conversation-Request-ID=%q want client-req-1 (inbound passthrough)", got)
	}
}

// TestChatAGlobalPathAndFallbackReuseConvReqID global realm /console → /v2 fallback：
// 同一 ChatStream 内两条候选路径出站复用同一 conversationRequestID（换路径不改聚合键）。
func TestChatAGlobalPathAndFallbackReuseConvReqID(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var headers []http.Header
	reqs := 0
	up := &upstream.Client{
		GlobalEnabled: true,
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reqs++
			headers = append(headers, r.Header.Clone())
			// 首次路径（console）404 → 触发 fallback 到 /v2。
			if reqs == 1 {
				return &http.Response{
					StatusCode: 404,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":404}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:     "https://fake.example",
		ChatBaseGlobal: "https://fake.example",
		BillingBaseCN:  "https://fake.example",
	}
	a := &auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (global /v2 fallback)", rec.Code, rec.Body)
	}
	if reqs != 2 {
		t.Fatalf("reqs=%d want 2 (console 404 → fallback /v2)", reqs)
	}
	if headers[0].Get("X-Conversation-Request-ID") == "" {
		t.Fatal("console attempt missing X-Conversation-Request-ID")
	}
	if headers[0].Get("X-Conversation-Request-ID") != headers[1].Get("X-Conversation-Request-ID") {
		t.Errorf("console vs /v2 fallback conversationRequestID differ: %q vs %q",
			headers[0].Get("X-Conversation-Request-ID"), headers[1].Get("X-Conversation-Request-ID"))
	}
	// global 侧头族同样完整：CN/global 同构。
	if headers[0].Get("X-B3-TraceId") == "" || headers[0].Get("X-B3-SpanId") == "" {
		t.Errorf("global console attempt missing B3 family: trace=%q span=%q",
			headers[0].Get("X-B3-TraceId"), headers[0].Get("X-B3-SpanId"))
	}
}

// isValidB3Trace 16/32 hex 校验（避免重复断言逻辑）。
func isValidB3Trace(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for _, ch := range s {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}