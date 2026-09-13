package main

import (
	"reflect"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// realmPool 构造含 cn/global 账号的池并临时打开 global 开关。
func realmPool(t *testing.T) *pool.Pool {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(false) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	return p
}

// TestRealmAwareAvailableForModel 断言会话粘性的 realm 感知闭包：
// 带前缀的模型名按 realm 过滤可用账号，裸名走 cn。
func TestRealmAwareAvailableForModel(t *testing.T) {
	p := realmPool(t)
	fn := realmAwareAvailableForModel(p)

	cases := []struct {
		model string
		want  []string
	}{
		{"glm-5.2", []string{"cn1"}},           // 裸名 → cn 集合
		{"cn:glm-5.2", []string{"cn1"}},        // 显式 cn 前缀 → cn 集合
		{"global:gpt-5.4", []string{"g1"}},     // global 前缀 → global 集合
	}
	for _, c := range cases {
		if got := fn(c.model); !reflect.DeepEqual(got, c.want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, c.want)
		}
	}
}

// TestRealmAwareAvailableForModelGlobalDisabled 零回归：GlobalEnabled=false（缺省纯 CN）
// 时，即便 auth 写了 realm=global 也不路由 global——闭包对 global: 前缀返回空集。
func TestRealmAwareAvailableForModelGlobalDisabled(t *testing.T) {
	// 注意：不调 SetGlobalEnabled(true) → 开关保持关闭（缺省）。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	fn := realmAwareAvailableForModel(p)

	// 开关未开 → 该 global 账号 Realm()=="cn"（双保险），对 global: 前缀不可见。
	if got := fn("global:gpt-5.4"); len(got) != 0 {
		t.Errorf("global disabled: AvailableForModel(global:gpt-5.4)=%v want empty", got)
	}
	// 裸名 → cn：该账号被当作 cn 可见（现状等价，零回归）。
	if got := fn("glm-5.2"); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("global disabled: AvailableForModel(glm-5.2)=%v want [g1]", got)
	}
}