package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestGlobalAccountsSkipCheckinTravelActivity 门控核心：池内 global 账号在
// checkin/travel/activity 三类循环中**零上游调用**；CN 账号照常。
//
// fake upstream 全路径统计调用数：global 账号若被误放行，会打到 fake server
// 的 /daily-checkin /buddy/info /v2/report 等任意路径，调用数即 >0。
func TestGlobalAccountsSkipCheckinTravelActivity(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected for global", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	// 显式 realm=global（下行 Domain 兜底场景在 auth 包单测覆盖，这里直接构造 global）。
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	// 三类循环各跑一趟：global 账号不应发起任何上游调用。
	s.CheckinAll()
	s.RunActivityNow()
	s.RunTravelNow()

	if n := calls.Load(); n != 0 {
		t.Errorf("global account upstream calls=%d want 0（checkin/travel/activity 均跳过）", n)
	}
}

// TestGlobalAccountSkippedListedWithStatus 门控跳过的 global 账号在 CheckinAll
// 回执里以 skipped(global) 呈现——手动触发时结果可读，不再是"未覆盖"的空白。
func TestGlobalAccountSkippedListedWithStatus(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected for global", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 || out[0].UID != "g1" {
		t.Fatalf("out=%+v want 1 row for g1", out)
	}
	if out[0].Status != CheckinSkipped {
		t.Errorf("status=%q want skipped（global 门控回执）", out[0].Status)
	}
	if out[0].Detail != "global" {
		t.Errorf("detail=%q want global", out[0].Detail)
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0", calls.Load())
	}
}

// TestGlobalAndCNMixedPoolOnlyCNServed 混池：global 账号被跳过、CN 账号照常跑。
// 一次 fake upstream 同时统计两类账号真正打到的请求——区隔"零调用"不是池空、
// 而是全局跳过逻辑生效。
func TestGlobalAndCNMixedPoolOnlyCNServed(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	stub := &reportStub{} // /v2/report 统计 + X-User-Id 判定
	growth := &travelStub{buddy: "null"}
	// 合并 growth/billing 端点：activity 的 report + travel 的 buddy 都要能被 CN 打到。
	both := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			stub.handler().ServeHTTP(w, r)
			return
		}
		growth.handler().ServeHTTP(w, r)
	}))
	defer both.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: both.Client(), ChatBaseCN: both.URL, BillingBaseCN: both.URL}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow()
	if n := stub.calls.Load(); n != 1 {
		t.Errorf("CN activity report calls=%d want 1（仅 CN 账号上报）", n)
	}
	// activity 阶段会顺带领猫（travelAdoptForce），把 buddy/info 计数打进快照；
	// 旅行检验只数 RunTravelNow 这段的增量。
	infoBefore := growth.infoCalls.Load()

	s.RunTravelNow()
	infoDelta := growth.infoCalls.Load() - infoBefore
	if infoDelta != 1 {
		t.Errorf("CN travel buddy-info delta=%d want 1（仅 CN 账号旅行）", infoDelta)
	}
}

// TestRunKeepaliveStillRefreshesGlobal keepalive 不拦 global：token refresh 端点
// 对 global 存在，刷新调用照发。
func TestRunKeepaliveStillRefreshesGlobal(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "g1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1,
		Domain: "www.workbuddy.ai"}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("global keepalive refresh calls=%d want 1（keepalive 不拦 global）", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("global token 未刷新: %s", a.AccessToken)
	}
}
