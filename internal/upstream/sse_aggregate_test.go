package upstream

import (
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
