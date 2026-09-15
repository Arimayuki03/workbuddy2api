// admin_test.go — /admin 端点与安全边界、config.json 最小 diff 写回的单测。
// 全程空池：积分查询/手动触发都不产生任何上游网络调用。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// baseConfig 一份"真实感" config.json：未知字段、嵌套段、2 空格缩进——写回路径必须原样保留。
const baseConfig = `{
  "listen": ":7863",
  "api_key": "sekret",
  "schedule": {
    "checkin_hours": [9, 21],
    "checkin_enabled": true,
    "keepalive_enabled": true
  },
  "upstream": {
    "timeout_seconds": 120
  },
  "retired_region": "cn"
}
`

func adminHandler(t *testing.T, apiKey, cfgPath string) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     pool.New(t.TempDir() + "/state.json"),
		Upstream: upstream.New(),
		APIKey:   apiKey,
		Sched:    scheduler.New(scheduler.Config{Pool: pool.New(t.TempDir() + "/s2.json"), Upstream: upstream.New()}),
		Admin:    AdminConfig{Enabled: true, ConfigPath: cfgPath},
	})
}

func do(t *testing.T, h *Handler, method, path, key, body string, remote string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	// httptest 默认 RemoteAddr 是文档 IP 192.0.2.1——/admin 的 loopback 闸会拒；
	// 测试语义即"本机插件"，缺省钉回 127.0.0.1，负例显式传 remote。
	if remote == "" {
		remote = "127.0.0.1:54321"
	}
	req.RemoteAddr = remote
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdminRoutesAbsentByDefault(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	for _, path := range []string{"/admin/tasks", "/admin/credits"} {
		if rec := do(t, h, "GET", path, "", "", ""); rec.Code != http.StatusNotFound {
			t.Fatalf("默认关闭时 %s 应 404，实得 %d", path, rec.Code)
		}
	}
	// 既有路由不受注册逻辑影响
	if rec := do(t, h, "GET", "/healthz", "", "", ""); rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz 意外状态 %d", rec.Code)
	}
}

func TestAdminTasksAuthAndBody(t *testing.T) {
	h := adminHandler(t, "sekret", "")
	if rec := do(t, h, "GET", "/admin/tasks", "", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key 应 401，实得 %d", rec.Code)
	}
	rec := do(t, h, "GET", "/admin/tasks", "wrong", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错 key 应 401，实得 %d", rec.Code)
	}
	rec = do(t, h, "GET", "/admin/tasks", "sekret", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("正确 key 应 200，实得 %d body=%s", rec.Code, rec.Body)
	}
	var v struct {
		Service string                   `json:"service"`
		Tasks   []scheduler.KindSnapshot `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Service != ServiceName || len(v.Tasks) != 6 {
		t.Fatalf("响应结构异常：%+v", v)
	}
	if v.Tasks[0].Kind != "checkin" || v.Tasks[0].Label == "" {
		t.Fatalf("tasks[0] 异常：%+v", v.Tasks[0])
	}
}

func TestAdminLoopbackOnly(t *testing.T) {
	h := adminHandler(t, "sekret", "")
	if rec := do(t, h, "GET", "/admin/tasks", "sekret", "", "192.168.1.2:54321"); rec.Code != http.StatusForbidden {
		t.Fatalf("LAN 来源即使带对 key 也应 403，实得 %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/admin/tasks", "sekret", "", "127.0.0.1:54321"); rec.Code != http.StatusOK {
		t.Fatalf("loopback 应放行，实得 %d", rec.Code)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:7863": true, "[::1]:7863": true, "127.1.2.3:1": true,
		"192.168.6.188:1": false, "10.0.0.1:2": false, "[::ffff:1.2.3.4]:80": false, "": false,
	}
	for a, want := range cases {
		if got := isLoopbackAddr(a); got != want {
			t.Errorf("isLoopbackAddr(%q)=%v 应为 %v", a, got, want)
		}
	}
}

func TestAdminTaskRun(t *testing.T) {
	h := adminHandler(t, "sekret", "")
	if rec := do(t, h, "POST", "/admin/tasks/run", "sekret", `{"kind":"nope"}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 kind 应 400，实得 %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/admin/tasks/run", "sekret", `{"kind":"checkin","typo":1}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应 400（防 PATCH 打错键静默没改），实得 %d", rec.Code)
	}
	rec := do(t, h, "POST", "/admin/tasks/run", "sekret", `{"kind":"checkin"}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("空池 checkin 应立即 202，实得 %d body=%s", rec.Code, rec.Body)
	}
	var v map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if got, _ := v["started"].([]any); len(got) != 1 || got[0] != "checkin" {
		t.Fatalf("started 异常：%v", v["started"])
	}
	// 白盒预置 manual 标记后 run all：全 busy → 409，且不会起任何 goroutine（尤其 school/cat 的 python 脚本）
	h.adm.mu.Lock()
	for _, k := range scheduler.Kinds() {
		h.adm.manual[k] = true
	}
	h.adm.mu.Unlock()
	if rec := do(t, h, "POST", "/admin/tasks/run", "sekret", `{"kind":"all"}`, ""); rec.Code != http.StatusConflict {
		t.Fatalf("全 busy 应 409，实得 %d body=%s", rec.Code, rec.Body)
	}
}

func TestAdminTaskPatchMinimalDiffAndBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(baseConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	h := adminHandler(t, "sekret", path)

	rec := do(t, h, "PATCH", "/admin/tasks", "sekret", `{"kind":"checkin","enabled":false}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH 应 200，实得 %d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["persisted"] != true {
		t.Fatalf("persisted 应为 true：%v", resp)
	}

	newRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bakRaw, err := os.ReadFile(path + ".bak")
	if err != nil || string(bakRaw) != baseConfig {
		t.Fatalf("备份缺失或与原件不符：err=%v equal=%v", err, string(bakRaw) == baseConfig)
	}
	// 最小 diff：逐行比对只允许目标行变化
	oldLines := strings.Split(baseConfig, "\n")
	newLines := strings.Split(string(newRaw), "\n")
	if len(oldLines) != len(newLines) {
		t.Fatalf("行数变了 %d→%d（应只改一行值）", len(oldLines), len(newLines))
	}
	diff := 0
	for i := range oldLines {
		if oldLines[i] != newLines[i] {
			diff++
			if !strings.Contains(newLines[i], `"checkin_enabled": false`) {
				t.Fatalf("变化的是非目标行：\nold %q\nnew %q", oldLines[i], newLines[i])
			}
		}
	}
	if diff != 1 {
		t.Fatalf("期望恰好 1 行变化，实得 %d", diff)
	}
	// 键序与未知字段保留
	if i := strings.Index(string(newRaw), `"retired_region"`); i < 0 {
		t.Fatal("未知字段丢失")
	}
	if strings.Index(string(newRaw), `"listen"`) > strings.Index(string(newRaw), `"schedule"`) {
		t.Fatal("顶层键序变了")
	}

	// 等值再 PATCH：persisted true + note 未改动 + 文件字节不变（也不新增备份差异）
	rec = do(t, h, "PATCH", "/admin/tasks", "sekret", `{"kind":"checkin","enabled":false}`, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["note"].(string), "已是目标值") {
		t.Fatalf("等值提交应注明未改动：%v", resp["note"])
	}
	if same, _ := os.ReadFile(path); string(same) != string(newRaw) {
		t.Fatal("等值提交不该改文件")
	}
}

func TestAdminTaskPatchInsertsMissingSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := "{\n  \"listen\": \":7863\"\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	h := adminHandler(t, "sekret", path)
	rec := do(t, h, "PATCH", "/admin/tasks", "sekret", `{"kind":"travel","enabled":false}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实得 %d body=%s", rec.Code, rec.Body)
	}
	raw, _ := os.ReadFile(path)
	if !json.Valid(raw) {
		t.Fatalf("写回后 JSON 非法：%s", raw)
	}
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	sch, _ := v["schedule"].(map[string]any)
	if sch == nil || sch["travel_enabled"] != false {
		t.Fatalf("schedule.travel_enabled 未写入：%s", raw)
	}
	if !strings.Contains(string(raw), `"listen": ":7863"`) {
		t.Fatal("原有键被改坏")
	}
}

func TestSpliceBoolTable(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		section string
		key     string
		lit     string
		wantErr bool
		check   func(t *testing.T, out []byte, changed bool)
	}{
		{"equal", `{"schedule":{"checkin_enabled": true}}`, "schedule", "checkin_enabled", "true", false,
			func(t *testing.T, out []byte, changed bool) {
				if changed || !bytes.Equal(out, []byte(`{"schedule":{"checkin_enabled": true}}`)) {
					t.Fatalf("等值应零改动 changed=%v out=%s", changed, out)
				}
			}},
		{"crlf", "{\r\n  \"schedule\": {\r\n    \"cat_enabled\": false\r\n  }\r\n}\r\n", "schedule", "cat_enabled", "true", false,
			func(t *testing.T, out []byte, changed bool) {
				s := string(out)
				if !strings.Contains(s, "\"cat_enabled\": true\r\n") || !strings.Contains(s, "}\r\n}\r\n") {
					t.Fatalf("CRLF 未被保留：%q", s)
				}
			}},
		{"empty top", "{}", "schedule", "checkin_enabled", "false", false,
			func(t *testing.T, out []byte, _ bool) {
				var v map[string]any
				if err := json.Unmarshal(out, &v); err != nil || v["schedule"].(map[string]any)["checkin_enabled"] != false {
					t.Fatalf("空顶层插入失败 err=%v out=%s", err, out)
				}
			}},
		{"empty section", `{"schedule":{}}`, "schedule", "school_enabled", "false", false,
			func(t *testing.T, out []byte, _ bool) {
				var v map[string]map[string]bool
				if err := json.Unmarshal(out, &v); err != nil {
					t.Fatalf("空 section 插入后应合法（不带尾逗号）err=%v out=%s", err, out)
				}
				if v["schedule"]["school_enabled"] != false {
					t.Fatalf("值不对：%s", out)
				}
			}},
		{"invalid json", `{"a":}`, "schedule", "k", "true", true, nil},
		{"top array", `[1,2]`, "schedule", "k", "true", true, nil},
		{"obj value rejected", `{"schedule":{"checkin_enabled":{"nested":true}}}`, "schedule", "checkin_enabled", "true", true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, changed, err := spliceBool([]byte(c.raw), c.section, c.key, []byte(c.lit))
			if c.wantErr {
				if err == nil {
					t.Fatalf("应报错，实得 out=%s changed=%v", out, changed)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			c.check(t, out, changed)
		})
	}
}

func TestAdminCreditsCooldownFlow(t *testing.T) {
	h := adminHandler(t, "sekret", "")
	h.cfg.Admin.CreditRefreshMinInterval = 60 * time.Second
	// 空池刷新：瞬时 200，零上游调用
	rec := do(t, h, "POST", "/admin/credits", "sekret", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("首次查询应 200，实得 %d body=%s", rec.Code, rec.Body)
	}
	var v map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v["cached"] != true {
		t.Fatalf("查询成功后 cached 应为 true：%v", v)
	}
	// 冷却期内再查：429
	rec = do(t, h, "POST", "/admin/credits", "sekret", "", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("冷却中应 429，实得 %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v["error"] != "credit_refresh_cooldown" {
		t.Fatalf("429 错误码异常：%v", v["error"])
	}
	// GET 回放缓存 + 冷却截止
	rec = do(t, h, "GET", "/admin/credits", "sekret", "", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	cu, _ := v["cooldown_until"].(float64)
	if v["cached"] != true || cu <= float64(time.Now().Unix()) {
		t.Fatalf("GET 回放异常：%v", v)
	}
	// 单飞：白盒置 running 再 POST → 429 running
	h.adm.mu.Lock()
	h.adm.creditRunning = true
	h.adm.mu.Unlock()
	rec = do(t, h, "POST", "/admin/credits", "sekret", "", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusTooManyRequests || v["error"] != "credit_refresh_running" {
		t.Fatalf("并发查询应 429 running，实得 %d %v", rec.Code, v["error"])
	}
}

func TestAdminShutdownCallback(t *testing.T) {
	called := make(chan struct{})
	h := adminHandler(t, "sekret", "")
	h.cfg.OnShutdown = func() { close(called) }
	rec := do(t, h, "POST", "/admin/shutdown", "sekret", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("shutdown 应先回 200，实得 %d", rec.Code)
	}
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("OnShutdown 回调未在 3s 内触发")
	}
}

func TestAdminPatchWithoutConfigPath(t *testing.T) {
	h := adminHandler(t, "sekret", "") // ConfigPath 空
	rec := do(t, h, "PATCH", "/admin/tasks", "sekret", `{"kind":"cat","enabled":false}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200（内存生效），实得 %d", rec.Code)
	}
	var v map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v["persisted"] != false || !strings.Contains(v["note"].(string), "仅内存") {
		t.Fatalf("无路径时应如实报告未落盘：%v", v)
	}
}
