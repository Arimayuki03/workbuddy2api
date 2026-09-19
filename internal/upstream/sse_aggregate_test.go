package upstream

import (
	"errors"
	"strings"
	"testing"
)

// 回归：Aggregate 的 gotAnyContent 只能由非空 content 置位。
// 有的上游把完整消息放在 choices[].message 里（非 delta），并以
// delta.content:"" 的角色切换片开场——空串若也置位 gotAnyContent，
// message.content 兜底分支会被吞掉，聚合结果 content 恒为空。
func TestAggregateEmptyContentDeltaKeepsMessageFallback(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{},"message":{"role":"assistant","content":"final answer"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	resp, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	chs, ok := resp["choices"].([]any)
	if !ok || len(chs) != 1 {
		t.Fatalf("choices shape: %#v", resp["choices"])
	}
	msg, _ := chs[0].(map[string]any)["message"].(map[string]any)
	if msg == nil {
		t.Fatalf("message missing: %#v", chs[0])
	}
	if got, _ := msg["content"].(string); got != "final answer" {
		t.Fatalf("content = %q, want %q（message.content 兜底被空 delta.content 吞掉）", got, "final answer")
	}
}

// 正例回归：真实 delta 内容仍正常聚合，兜底分支不重复写入。
func TestAggregateRealContentStillAggregated(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c2","model":"m","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c2","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	resp, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	chs := resp["choices"].([]any)
	msg := chs[0].(map[string]any)["message"].(map[string]any)
	if got, _ := msg["content"].(string); got != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}
}

// 回归（审计）：非流式 Aggregate 遇到流内上游 error 帧必须作为上游错误返回，
// 不能静默吞掉后聚合成 HTTP 200 + 空 content + 伪造 finish_reason:"stop" 的假
// 成功响应（流式 Stream 对同样输入会如实透传错误帧）。识别判据与 Stream 的
// error-passthrough 一致（带 error 键的 JSON 帧），分类复用 Stream 侧流内错误帧
// 的既有判定 FrameKind（6004 模型级限流 → soft_rate），Msg 装 payload 原文。
// 即便此前已聚合到部分 content，错误仍然获胜（不把半截成功响应交给客户端）。
func TestAggregateErrorFrameReturnsUpstreamError(t *testing.T) {
	const errFrame = `{"error":{"message":"您的使用量已超出频率限制","code":"6004","requestId":"req-rl-42"}}`
	raw := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"}}]}\n\n" +
		"data: " + errFrame + "\n\n" +
		"data: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(raw))
	if err == nil {
		t.Fatalf("expected upstream error, got resp=%v（error 帧被吞成正常聚合）", resp)
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("error should be *Error envelope, got %T: %v", err, err)
	}
	// 与 Stream 侧 FrameKind 同判据：6004 → ErrSoftRate（模型级限流）。
	if ue.Kind != ErrSoftRate {
		t.Errorf("Kind=%v want soft_rate（6004 模型级限流）", ue.Kind)
	}
	// 上游原文透传：message/code/requestId 原文都在 Msg 里。
	for _, want := range []string{"您的使用量已超出频率限制", `"code":"6004"`, `"requestId":"req-rl-42"`} {
		if !strings.Contains(ue.Msg, want) {
			t.Errorf("Msg missing %q: %s", want, ue.Msg)
		}
	}
}

// 回归（审计）补形态：判不出类别的 error 帧（message 无任何已知关键词）仍是
// 上游错误、原文透传——Kind 落 FrameKind 的既有兜底（error.message 走 Classify
// 的请求级 400 口径 → ErrClient），绝不静默聚合成功。
func TestAggregateUnclassifiedErrorFrameStillErrors(t *testing.T) {
	raw := "data: {\"error\":{\"message\":\"something exploded\",\"code\":\"99999\"}}\n\n" +
		"data: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(raw))
	if err == nil {
		t.Fatalf("expected upstream error, got resp=%v", resp)
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("error should be *Error envelope, got %T: %v", err, err)
	}
	if ue.Kind != ErrClient {
		t.Errorf("Kind=%v want client（FrameKind 对无关键词 message 的既有 400 口径兜底）", ue.Kind)
	}
	if !strings.Contains(ue.Msg, "something exploded") {
		t.Errorf("Msg must carry upstream原文: %s", ue.Msg)
	}
}

// 回归（审计）：validEvents 收紧——纯 id/空壳帧（只有 id/role/finish_reason，
// 无 delta.content/reasoning_content/tool_calls/message.content/usage 等实质
// 字段）不计为有效事件，全空壳帧流必须落入空流哨兵（与空流同报错），而不是被
// 当成正常响应合成 200 + 空 content + 伪造 finish_reason:"stop"。
func TestAggregateShellFramesHitEmptyStreamSentinel(t *testing.T) {
	raw := "data: {\"id\":\"z\"}\n\n" +
		"data: {\"id\":\"z\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"z\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(raw))
	if err == nil || !strings.Contains(err.Error(), "no valid data events") {
		t.Fatalf("shell-only stream must hit empty-stream sentinel, got resp=%v err=%v", resp, err)
	}
}

// 正例回归：空壳帧混入真实内容流不影响聚合（收紧只影响「全空壳」流；
// finish_reason/role 照常采集，只是不计入有效事件）。
func TestAggregateShellFramesMixedWithContentStillAggregated(t *testing.T) {
	raw := "data: {\"id\":\"z\"}\n\n" +
		"data: {\"id\":\"z\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"z\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hi" {
		t.Errorf("content=%q want hi", msg["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v want stop（空壳帧的 finish_reason 照常采集）", choice["finish_reason"])
	}
}
