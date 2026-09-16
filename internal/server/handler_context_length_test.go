package server

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestModelListContextLengthThreeLevelLookup CN 动态分支 context_length/max_output_tokens
// 三级查找（context_catalog）端到端：动态值权威 → 知识表 → 1M 兜底；零值不透出假 131072。
func TestModelListContextLengthThreeLevelLookup(t *testing.T) {
	resetModelsCache()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"dyn-full","maxInputTokens":65536,"maxOutputTokens":8192},
			{"id":"glm-5.2","maxInputTokens":0,"maxOutputTokens":0},
			{"id":"never-seen-model","maxInputTokens":0,"maxOutputTokens":0},
			{"id":"auto","maxInputTokens":0,"maxOutputTokens":0}
		],"agents":[{"name":"cli","models":["dyn-full","glm-5.2","never-seen-model","auto"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	byID := map[string]map[string]any{}
	for _, m := range h.modelList() {
		if id, ok := m["id"].(string); ok {
			byID[id] = m
		}
	}
	// 1) 动态值优先：远端 65536/8192 权威透出（知识表无 dyn-full，但远端有值即最优先）。
	if c := byID["cn:dyn-full"]["context_length"]; c != int64(65536) {
		t.Errorf("dyn-full context_length=%v want 65536 (remote wins)", c)
	}
	if o := byID["cn:dyn-full"]["max_output_tokens"]; o != int64(8192) {
		t.Errorf("dyn-full max_output_tokens=%v want 8192 (remote wins)", o)
	}
	// 2) 知识表命中：glm-5.2 上游零值 → 表值 1M/131072（fork 实测）。
	if c := byID["cn:glm-5.2"]["context_length"]; c != int64(1000000) {
		t.Errorf("glm-5.2 context_length=%v want 1000000 (knowledge table)", c)
	}
	if o := byID["cn:glm-5.2"]["max_output_tokens"]; o != int64(131072) {
		t.Errorf("glm-5.2 max_output_tokens=%v want 131072 (knowledge table)", o)
	}
	// 3) 未知 → 1M 兜底；max_output_tokens 未知省略。
	if c := byID["cn:never-seen-model"]["context_length"]; c != int64(1000000) {
		t.Errorf("unknown context_length=%v want 1000000 (1M fallback)", c)
	}
	if _, ok := byID["cn:never-seen-model"]["max_output_tokens"]; ok {
		t.Error("unknown model max_output_tokens must be omitted")
	}
	// 4) 知识表条目但输出上限未知（auto）→ context 走表 168000，输出省略。
	if c := byID["cn:auto"]["context_length"]; c != int64(168000) {
		t.Errorf("auto context_length=%v want 168000 (knowledge table)", c)
	}
	if _, ok := byID["cn:auto"]["max_output_tokens"]; ok {
		t.Error("auto max_output_tokens must be omitted (output unknown)")
	}
	// 全表扫描：任何条目都不得再出现 131072 假兜底充当 context_length
	//（dyn-full 的 65536 等真实值不受影响；本 fixture 无真 131072 值）。
	for id, m := range byID {
		if c, _ := m["context_length"].(int64); c == 131072 {
			t.Errorf("%s context_length=131072 (legacy fake fallback leaked)", id)
		}
	}
}

// TestModelListGlobalContextLengthLookup global 分支同口径：窄表裸 ID 条目经知识表
// 补齐（gpt-5.4 → 1050000）；探测富条目真实值权威；未知模型 1M 兜底。
func TestModelListGlobalContextLengthLookup(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, `{"code":0,"data":{"models":[
		{"id":"gpt-5.4","maxInputTokens":400000,"maxOutputTokens":100000}
	],"agents":[{"name":"cli","models":["gpt-5.4"]}]}}`)
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	byID := map[string]map[string]any{}
	for _, m := range h.modelList() {
		if id, ok := m["id"].(string); ok && len(id) > 7 && id[:7] == "global:" {
			byID[id] = m
		}
	}
	// CN fake 自带 cn-dyn-model（65536 真实值），global 探测产出 gpt-5.4。
	if len(byID) == 0 {
		t.Fatal("no global entries from probe")
	}
	// 富条目真实值权威（400000/100000 远端下发，覆盖知识表 1050000/128000）。
	if c := byID["global:gpt-5.4"]["context_length"]; c != int64(400000) {
		t.Errorf("gpt-5.4 context_length=%v want 400000 (probe value wins)", c)
	}
	if o := byID["global:gpt-5.4"]["max_output_tokens"]; o != int64(100000) {
		t.Errorf("gpt-5.4 max_output_tokens=%v want 100000 (probe value wins)", o)
	}
}

// TestModelListGlobalNarrowContextLookup 窄表探测（纯 ID 数组）→ 裸条目 context_length
// 经知识表补齐（不再裸 131072）；不在表中的窄表 ID → 1M 兜底。
func TestModelListGlobalNarrowContextLookup(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, `{"code":0,"data":["gpt-5.6-luna","narrow-unknown"]}`)
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	byID := map[string]map[string]any{}
	for _, m := range h.modelList() {
		if id, ok := m["id"].(string); ok && len(id) > 7 && id[:7] == "global:" {
			byID[id] = m
		}
	}
	if c := byID["global:gpt-5.6-luna"]["context_length"]; c != int64(1050000) {
		t.Errorf("narrow gpt-5.6-luna context_length=%v want 1050000 (knowledge table, not 131072)", c)
	}
	if c := byID["global:narrow-unknown"]["context_length"]; c != int64(1000000) {
		t.Errorf("narrow-unknown context_length=%v want 1000000 (unknown → 1M fallback)", c)
	}
	if _, ok := byID["global:narrow-unknown"]["max_output_tokens"]; ok {
		t.Error("narrow-unknown max_output_tokens must be omitted")
	}
}
