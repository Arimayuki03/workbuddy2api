package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// withGlobalEnabled 临时打开 global realm 开关，测试结束复位（默认关闭）。
func withGlobalEnabled(t *testing.T) {
	t.Helper()
	globalEnabled.Store(true)
	t.Cleanup(func() { globalEnabled.Store(false) })
}

func TestRealmExplicitGlobal(t *testing.T) {
	withGlobalEnabled(t)
	a := &Auth{realm: "global"}
	if got := a.Realm(); got != "global" {
		t.Errorf("Realm()=%q want global", got)
	}
	if !a.IsGlobal() {
		t.Error("IsGlobal()=false want true")
	}
}

func TestRealmExplicitCN(t *testing.T) {
	a := &Auth{realm: "cn"}
	if got := a.Realm(); got != "cn" {
		t.Errorf("Realm()=%q want cn", got)
	}
	if a.IsGlobal() {
		t.Error("IsGlobal()=true want false")
	}
}

func TestRealmDomainFallback(t *testing.T) {
	withGlobalEnabled(t)
	cases := []struct{ domain, want string }{
		{"www.workbuddy.ai", "global"},
		{"workbuddy.ai", "global"},
		{"sub.workbuddy.ai", "global"},
		{"www.codebuddy.cn", "cn"},
		{"", "cn"},
	}
	for _, c := range cases {
		a := &Auth{Domain: c.domain}
		if got := a.Realm(); got != c.want {
			t.Errorf("Domain=%q Realm()=%q want %q", c.domain, got, c.want)
		}
	}
}

func TestRealmEmptyFallsBackToCN(t *testing.T) {
	// 空 realm + 空 domain → cn（老 CN 凭证零回归的核心）。
	a := &Auth{}
	if got := a.Realm(); got != "cn" {
		t.Errorf("Realm()=%q want cn", got)
	}
	// 显式 global 但开关未开 → 恒 cn（双保险，D5）。
	ag := &Auth{realm: "global"}
	if got := ag.Realm(); got != "cn" {
		t.Errorf("global disabled: Realm()=%q want cn", got)
	}
	// domain 回落同样受开关约束。
	ad := &Auth{Domain: "www.workbuddy.ai"}
	if got := ad.Realm(); got != "cn" {
		t.Errorf("global disabled+domain: Realm()=%q want cn", got)
	}
}

func TestParseNestedRealm(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":"www.workbuddy.ai","realm":"global"},"account":{"uid":"u1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.realm != "global" {
		t.Errorf("nested realm=%q want global", sa.realm)
	}
	// 嵌套形显式 realm 空 → 读不到，靠 domain 回落。
	raw2 := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1},"account":{"uid":"u1"}}`)
	sa2, err := Parse(raw2)
	if err != nil {
		t.Fatalf("nested no-realm parse err: %v", err)
	}
	if sa2.realm != "" {
		t.Errorf("nested missing realm key should be zero, got %q", sa2.realm)
	}
}

func TestParseFlatRealm(t *testing.T) {
	withGlobalEnabled(t)
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u2","realm":"global"}`)
	fa, err := Parse(raw)
	if err != nil {
		t.Fatalf("flat parse err: %v", err)
	}
	if fa.realm != "global" {
		t.Errorf("flat realm=%q want global", fa.realm)
	}
	if !fa.IsGlobal() {
		t.Error("flat global account IsGlobal()=false want true")
	}
	// 扁平形缺 realm 键 → 零值 → CN。
	flatCN := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u3"}`)
	fc, err := Parse(flatCN)
	if err != nil {
		t.Fatalf("flat cn parse err: %v", err)
	}
	if fc.realm != "" {
		t.Errorf("flat missing realm should be zero, got %q", fc.realm)
	}
}

func TestSaveAtomicWritesRealm(t *testing.T) {
	withGlobalEnabled(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-global.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1,
		UID: "g1", realm: "global", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.realm != "global" {
		t.Errorf("roundtrip realm=%q want global", b.realm)
	}
	if !b.IsGlobal() {
		t.Error("roundtrip global account IsGlobal()=false")
	}
}