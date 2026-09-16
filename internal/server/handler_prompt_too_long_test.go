package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// handler_prompt_too_long_test.go 11115 prompt too long 三组端到端验收
// （任务书 prompt-too-long）：
//  1. 上游真实 11115 → 分类不罚号不轮转、透传原文（非固定文案）；
//  2. 预估 ≥95% → 不打上游、返回与上游 11115 逐字段同构的错误体（estimated 标注）；
//  3. 预估 <95% 正常长度 → 不误杀（放行打上游）。

// TestChatPromptTooLongPassesThroughUpstreamBody 上游 400 + 11115（含真实 token
// 数/上限值/requestId 原文）→ 不轮转（单号请求，calls=1）、不罚号（无冷却/禁用/
// 熔断计数）、error.message 透传上游原文（任务书 §1：用户修正——上游原文是
// 最有价值的错误信息，客户端必须看到，禁止固定词覆盖）。
func TestChatPromptTooLongPassesThroughUpstreamBody(t *testing.T) {
	const raw = `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"req-11115-abc"}`
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, raw, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	// message 必须逐字等于上游原文（透传，非固定文案）。
	if e.Error.Message != raw {
		t.Errorf("message=%q want raw upstream body passthrough %q", e.Error.Message, raw)
	}
	if !strings.Contains(e.Error.Message, "120000 tokens > 65536 maximum") || !strings.Contains(e.Error.Message, "req-11115-abc") {
		t.Errorf("message must preserve real token count/limit/requestId: %s", e.Error.Message)
	}
	if strings.Contains(e.Error.Message, "all accounts are temporarily unavailable") {
		t.Errorf("message must NOT be the fixed local scheduling text: %s", e.Error.Message)
	}
	// 不轮转：多账号池也只打第一个号（换号同样超限，白扔配额）。
	if calls["Bearer at1"]+calls["Bearer at2"] != 1 {
		t.Errorf("upstream calls=%v want exactly 1 (no rotation on 11115)", calls)
	}
	// 不罚号：a1 无冷却/无禁用/无熔断计数。
	st, _ := p.Status("a1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("11115 must not penalize account (request-level error): %+v", st)
	}
}

// TestChatPromptTooLongSingleAccount 413/404 状态码上的 11115 同样走透传分支
// （promptTooLongRule 认请求级 4xx 家族）。
func TestChatPromptTooLongSingleAccount(t *testing.T) {
	for _, status := range []int{400, 404, 413} {
		raw := `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"r"}`
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			return status, raw, false
		})
		h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatalf("status=%d: resp not json: %v body=%s", status, err, rec.Body)
		}
		if e.Error.Message != raw {
			t.Errorf("status=%d: message=%q want passthrough %q", status, e.Error.Message, raw)
		}
	}
}

// TestChatPromptEstimateBlocksBeforeUpstream 预估 ≥95%（任务书 §2）：不打上游
// （calls=0）、不选号（不占在途名额）、错误体与上游 11115 逐字段同构——
// code=11115、msg 同句式 + "(estimated)" 标注、requestId 本地生成（32-hex 形态）。
// 模型上限走查找链（glm-5.2 静态表 1M——真实场景该值从动态目录/查找链来，
// 这里用静态表口径构造确定上限）。
func TestChatPromptEstimateBlocksBeforeUpstream(t *testing.T) {
	upstream.ResetLookupChainForTest()
	resetModelsCache()
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	// 预估口径：ASCII 4 chars/token。glm-5.2 上限 = 静态表 1000000；
	// 构造 1,000,000 token（4M ASCII 字符）→ ratio=100% ≥95% → 拦截。
	big := strings.Repeat("a", 4_000_000)
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"` + big + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400 (pre-emptive 11115)", rec.Code, rec.Body)
	}
	if calls != 0 {
		t.Errorf("upstream must not be called on pre-emptive block, got %d", calls)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Code != "prompt_too_long" {
		t.Errorf("code=%q want prompt_too_long", e.Error.Code)
	}
	// message 是上游 11115 同构体：code=11115 + msg 句式 + (estimated) + requestId 32-hex。
	var iso struct {
		Code      int    `json:"code"`
		Msg       string `json:"msg"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal([]byte(e.Error.Message), &iso); err != nil {
		t.Fatalf("message body not isomorphic json: %v msg=%s", err, e.Error.Message)
	}
	if iso.Code != 11115 {
		t.Errorf("iso code=%d want 11115 (upstream code, field-isomorphic)", iso.Code)
	}
	if !strings.Contains(iso.Msg, "prompt is too long") ||
		!strings.Contains(iso.Msg, "1000000 tokens") ||
		!strings.Contains(iso.Msg, "maximum") ||
		!strings.Contains(iso.Msg, "(estimated)") {
		t.Errorf("iso msg=%q must follow upstream sentence + (estimated): prompt is too long: N tokens > L maximum (estimated)", iso.Msg)
	}
	if len(iso.RequestID) != 32 {
		t.Errorf("requestId len=%d want 32-hex (locally generated, same shape as upstream)", len(iso.RequestID))
	}
	// 不选号/不罚号：请求没打到任何账号。
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("pre-emptive block must not penalize account: %+v", st)
	}
}

// TestChatPromptEstimatePassesNormalLength 正常长度请求（远低于阈值）不误杀：
// 预估 <85% → 放行打上游，200 正常返回（任务书单测项「不误杀正常长度」）。
func TestChatPromptEstimatePassesNormalLength(t *testing.T) {
	upstream.ResetLookupChainForTest()
	resetModelsCache()
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	// 小请求：100 token（400 chars）对 1M 上限 → 0.01%，远低于 85%。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"`+strings.Repeat("a", 400)+`"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s want 200 (normal length must not be blocked)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1 (pass through)", calls)
	}
}

// TestChatPromptEstimateUnknownModel1MFallback 未知模型 → 查找链 1M 兜底口径
// 预判（不误杀）：4M 字符 → 1M token 预估 = 100% 上限 → 拦截同构体返回，
// 上限值展示 1000000（1M 兜底，任务书 §2「模型上限未知（1M 兜底）时按兜底值算」）。
func TestChatPromptEstimateUnknownModel1MFallback(t *testing.T) {
	upstream.ResetLookupChainForTest()
	resetModelsCache()
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	big := strings.Repeat("a", 4_000_000)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"totally-unknown-model","messages":[{"role":"user","content":"`+big+`"}]}`)))
	if calls != 0 {
		t.Fatalf("upstream calls=%d want 0 (1M fallback estimate blocks)", calls)
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v", err)
	}
	if !strings.Contains(e.Error.Message, "1000000 maximum") {
		t.Errorf("message=%s must show 1M fallback limit", e.Error.Message)
	}
}

// TestChatPromptEstimateNoUpstreamFetch 预估层零网络放大（关键设计约束）：
// 动态模型缓存 miss（fetchDynamicModels 会打上游探测）时预估**不得**触发探测
// ——否则每个 chat 请求都放大成 +2 次模型目录调用。fake 对非 chat 路径直接失败，
// 若预估触发 fetch 会 panic/报错到响应；本测试断言 chat 正常 200 且模型端点零调用。
func TestChatPromptEstimateNoUpstreamFetch(t *testing.T) {
	upstream.ResetLookupChainForTest()
	resetModelsCache()
	var modelFetches int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		// fake 不分路径——用 roundTripFunc 层面拦截更准；此处用调用计数兜底断言。
		modelFetches++
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s want 200", rec.Code, rec.Body)
	}
	// 1 次 = 只有 chat 调用（预估计层未触发任何模型目录探测）。
	if modelFetches != 1 {
		t.Errorf("fake calls=%d want 1 (estimate layer must not trigger model fetch)", modelFetches)
	}
}

// TestChatPromptEstimateCustomMaxCompletionTokensCoexists PR #116 整合回归：
// 带别名的请求正常流式转发，出站 body 已翻译 max_tokens（fake 捕获出站 body
// 验证——见 payload 层单测，此处只验证不因 11115/别名翻译破坏主链路）。
func TestChatPromptEstimateCustomMaxCompletionTokensCoexists(t *testing.T) {
	upstream.ResetLookupChainForTest()
	resetModelsCache()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":128000}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s want 200 (alias translation must not break main path)", rec.Code, rec.Body)
	}
}
