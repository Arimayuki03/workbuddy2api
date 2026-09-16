package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestClassifyPromptTooLong 11115「prompt is too long」专项分类（任务书 §1）：
// 请求级 4xx + 11115 code/文案 → ErrPromptTooLong；429/5xx/403 各自状态码语义
// 优先；未知 body 不误判。
func TestClassifyPromptTooLong(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		// 实测形态（任务书样本）：code=11115 + msg 文案。
		{400, `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"req-abc-123"}`, ErrPromptTooLong},
		// 仅 msg 文案（无 code 字段的形态，大小写不敏感）。
		{400, `{"msg":"Prompt is too long: 100 tokens > 50 maximum"}`, ErrPromptTooLong},
		// 字符串 code 形态。
		{400, `{"code":"11115","msg":"prompt is too long"}`, ErrPromptTooLong},
		// 404 / 413 请求级状态码同样命中。
		{404, `{"code":11115,"msg":"prompt is too long"}`, ErrPromptTooLong},
		{413, `{"code":11115,"msg":"prompt is too long"}`, ErrPromptTooLong},
		// 429 + 11115：限流语义优先（promptTooLongRule 只认请求级 4xx）。
		{429, `{"code":11115,"msg":"prompt is too long"}`, ErrSoftRate},
		// 5xx + 文案：服务端故障优先。
		{500, `{"code":11115,"msg":"prompt is too long"}`, ErrServer},
		// 403 带业务信封：不含 11115 → 不归 promptTooLong（该形态落 ErrClient）。
		{403, `{"code":1,"msg":"unknown business error"}`, ErrClient},
		// 11115 撞在 requestId 上不算（与 11102 撞 ID 同坑：裸 "11115" 子串不在词表，
		// 只认 `"code":11115` 字段形态与 msg 文案）。
		{400, `{"requestId":"11115","msg":"ok"}`, ErrClient},
		// 普通参数错误不受影响。
		{400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, ErrBadParams},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestIsPromptTooLong 直接回归判定入口：请求级 4xx 命中、其他状态码恒 false。
func TestIsPromptTooLong(t *testing.T) {
	if !IsPromptTooLong(400, `{"code":11115,"msg":"prompt is too long"}`) {
		t.Error("400 + 11115 envelope must be prompt-too-long")
	}
	if IsPromptTooLong(429, `{"code":11115,"msg":"prompt is too long"}`) {
		t.Error("429 must not be prompt-too-long (rate-limit semantics win)")
	}
	if IsPromptTooLong(200, `{"code":11115,"msg":"prompt is too long"}`) {
		t.Error("200 must not be prompt-too-long")
	}
	if IsPromptTooLong(400, `{"code":11101,"msg":"x"}`) {
		t.Error("11101 must not be prompt-too-long")
	}
}

// TestPromptEstimateTokens 估算口径：ASCII 4 字符/token、CJK 1 字符/token、
// content 数组 text 分段计入、非 text 分段/坏 body 零值。
func TestPromptEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"ascii string content", `{"messages":[{"role":"user","content":"aaaaaaaa"}]}`, 2},        // 8/4
		{"cjk string content", `{"messages":[{"role":"user","content":"你好世界"}]}`, 4},          // 4 CJK × 1
		{"mixed", `{"messages":[{"role":"user","content":"aabb你好"}]}`, 1 + 2},                // 4 ascii/4 + 2 CJK
		{"multi messages", `{"messages":[{"content":"aaaa"},{"content":"bbbb"}]}`, 2},          // 2+2... 4/4 + 4/4 = 2
		{"content array text segs", `{"messages":[{"content":[{"type":"text","text":"aaaaaaaa"},{"type":"image_url","image_url":{"url":"x"}}]}]}`, 2},
		{"no messages", `{"model":"m"}`, 0},
		{"bad json", `not json`, 0},
		{"empty body", ``, 0},
	}
	for _, c := range cases {
		if got := PromptEstimateTokens([]byte(c.body)); got != c.want {
			t.Errorf("%s: PromptEstimateTokens=%d want %d", c.name, got, c.want)
		}
	}
}

// TestEstimatePromptTokensThresholds 95%/85% 阈值边界（任务书实现约束）：
// 95% 拦截、85% WARN、84.9% 放行、limit 未知/预估为零恒放行。
func TestEstimatePromptTokensThresholds(t *testing.T) {
	// 构造 body：limit=1000 时预估 = N 个 "a"（每 4 个 1 token）。
	bodyFor := func(tokens int64) []byte {
		return []byte(`{"messages":[{"content":"` + strings.Repeat("a", int(tokens*4)) + `"}]}`)
	}
	if r := EstimatePromptTokens(bodyFor(950), "m", 1000); !r.ShouldBlock || r.ShouldWarn {
		t.Errorf("950/1000=95%%: block=%v warn=%v want block=true", r.ShouldBlock, r.ShouldWarn)
	}
	if r := EstimatePromptTokens(bodyFor(1000), "m", 1000); !r.ShouldBlock {
		t.Errorf("1000/1000=100%%: block=%v want true", r.ShouldBlock)
	}
	if r := EstimatePromptTokens(bodyFor(949), "m", 1000); r.ShouldBlock || !r.ShouldWarn {
		t.Errorf("949/1000=94.9%%: block=%v warn=%v want warn=true (below block)", r.ShouldBlock, r.ShouldWarn)
	}
	if r := EstimatePromptTokens(bodyFor(850), "m", 1000); r.ShouldBlock || !r.ShouldWarn {
		t.Errorf("850/1000=85%%: block=%v warn=%v want warn=true", r.ShouldBlock, r.ShouldWarn)
	}
	if r := EstimatePromptTokens(bodyFor(849), "m", 1000); r.ShouldBlock || r.ShouldWarn {
		t.Errorf("849/1000=84.9%%: both false (pass through), got block=%v warn=%v", r.ShouldBlock, r.ShouldWarn)
	}
	// limit 未知 → 恒放行（不误杀）。
	if r := EstimatePromptTokens(bodyFor(990), "m", 0); r.ShouldBlock || r.ShouldWarn {
		t.Errorf("limit=0 must pass through, got block=%v warn=%v", r.ShouldBlock, r.ShouldWarn)
	}
	// 预估为零（无文本）→ 恒放行。
	if r := EstimatePromptTokens([]byte(`{"messages":[]}`), "m", 1000); r.ShouldBlock || r.ShouldWarn {
		t.Errorf("no text must pass through, got block=%v warn=%v", r.ShouldBlock, r.ShouldWarn)
	}
}

// TestBuildPromptTooLongBodyIsomorphic 预防性错误体与上游真实 11115 逐字段同构
// （任务书实现约束）：code=11115、msg 同句式 + "(estimated)" 标注、requestId 32-hex；
// 字段集与上游样本完全一致（不新增字段、不发明新词汇）。
func TestBuildPromptTooLongBodyIsomorphic(t *testing.T) {
	const upstreamSample = `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"req-abc-123"}`
	got := BuildPromptTooLongBody(120000, 65536, "req-abc-123")

	var gotEnv, wantEnv struct {
		Code      int    `json:"code"`
		Msg       string `json:"msg"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal([]byte(got), &gotEnv); err != nil {
		t.Fatalf("built body not json: %v body=%s", err, got)
	}
	if err := json.Unmarshal([]byte(upstreamSample), &wantEnv); err != nil {
		t.Fatalf("sample not json: %v", err)
	}
	// code 逐字段同构。
	if gotEnv.Code != wantEnv.Code || gotEnv.Code != 11115 {
		t.Errorf("code=%d want 11115 (same as upstream)", gotEnv.Code)
	}
	// requestId 同字段同形态（本地生成 32-hex 也同构）。
	if gotEnv.RequestID != "req-abc-123" {
		t.Errorf("requestId=%q want passthrough of provided id", gotEnv.RequestID)
	}
	// msg 同句式：上游原文 + " (estimated)" 标注（唯一有意差异）。
	if wantEnv.Msg+" (estimated)" != gotEnv.Msg {
		t.Errorf("msg=%q want upstream sentence + \" (estimated)\" = %q", gotEnv.Msg, wantEnv.Msg+" (estimated)")
	}
	// 字段集一致：键名逐一比对（不新增字段）。
	var gotKeys, wantKeys []string
	for k := range mustJSONMap(t, got) {
		gotKeys = append(gotKeys, k)
	}
	for k := range mustJSONMap(t, upstreamSample) {
		wantKeys = append(wantKeys, k)
	}
	if len(gotKeys) != len(wantKeys) {
		t.Errorf("field count %d want %d (no extra fields invented)", len(gotKeys), len(wantKeys))
	}
}

func mustJSONMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("not json: %v", err)
	}
	return m
}

// TestGenRequestID32Hex 本地 requestId 形态：32-hex 非空，两次生成不同。
func TestGenRequestID32Hex(t *testing.T) {
	a, b := GenRequestID32Hex(), GenRequestID32Hex()
	if len(a) != 32 || len(b) != 32 {
		t.Fatalf("len=%d/%d want 32", len(a), len(b))
	}
	if a == b {
		t.Errorf("two generations must differ: %s", a)
	}
	for _, c := range a {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Errorf("non-hex char %q in %s", c, a)
		}
	}
}
