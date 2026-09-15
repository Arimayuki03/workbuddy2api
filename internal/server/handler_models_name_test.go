package server

import (
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestModelListNameFieldDynamicCN 动态分支：上游下发的 name（显示名）透出到 /v1/models；
// 上游省略 name 的模型整体省略该字段（不输出空字符串，不编造）。
func TestModelListNameFieldDynamicCN(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"dyn-named","name":"Hunyuan T1","maxInputTokens":65536,"maxOutputTokens":8192},
			{"id":"dyn-unnamed","maxInputTokens":65536,"maxOutputTokens":8192}
		],"agents":[{"name":"cli","models":["dyn-named","dyn-unnamed"]}]}}`, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	got := h.modelList()
	byID := map[string]map[string]any{}
	for _, m := range got {
		if id, ok := m["id"].(string); ok {
			byID[id] = m
		}
	}
	if name := byID["cn:dyn-named"]["name"]; name != "Hunyuan T1" {
		t.Errorf("dyn-named name=%v want Hunyuan T1", name)
	}
	if _, ok := byID["cn:dyn-unnamed"]["name"]; ok {
		t.Error("dyn-unnamed should omit name field (upstream omitted)")
	}
}

// TestModelListNameFieldStaticCNOmited 静态兜底表无 name 数据源 → 字段省略（不编造）。
func TestModelListNameFieldStaticCNOmited(t *testing.T) {
	resetModelsCache()
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: upstream.New(), GlobalEnabled: false})
	for _, m := range h.modelList() {
		if _, ok := m["name"]; ok {
			t.Errorf("static CN entry should not carry name (no data source): %v", m["id"])
		}
	}
}

// TestModelListNameFieldGlobalOmited global 分支只有 ID 名单（FetchGlobalModels 只产名），
// 无 name 数据源 → 字段省略（不编造）。
func TestModelListNameFieldGlobalOmited(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	cf := newGlobalModelsHandlerFake(t, 500, `{"code":500,"msg":"boom"}`) // 探测失败→静态名单
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})
	for _, m := range h.modelList() {
		if id, _ := m["id"].(string); len(id) > 6 && id[:6] == "global" {
			if _, ok := m["name"]; ok {
				t.Errorf("global entry should not carry name (no data source): %v", id)
			}
		}
	}
}
