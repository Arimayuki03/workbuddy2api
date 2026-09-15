// admin_test.go — /admin 配套能力（热开关 / 快照 / 手动触发互斥 / 观测记录）单测。
// 全程空池 + 无上游：任何任务体都零网络调用。
package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func newTestSched(t *testing.T, cfg Config) *Scheduler {
	t.Helper()
	cfg.Pool = pool.New(t.TempDir() + "/state.json")
	cfg.Upstream = upstream.New()
	return New(cfg)
}

func TestKindNamesAndParse(t *testing.T) {
	names := Kinds()
	if len(names) != kindCount {
		t.Fatalf("Kinds()=%d 类，应为 %d", len(names), kindCount)
	}
	want := []string{"checkin", "travel", "activity", "keepalive", "school", "cat"}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("Kinds()[%d]=%q 应为 %q", i, names[i], w)
		}
		k, ok := KindFromName(w)
		if !ok || int(k) != i {
			t.Fatalf("KindFromName(%q) = %v,%v", w, k, ok)
		}
	}
	if _, ok := KindFromName("all"); ok {
		t.Fatal("all 不是具体 kind，KindFromName 必须拒绝")
	}
}

func TestSnapshotAllDefaults(t *testing.T) {
	s := newTestSched(t, Config{}) // 零值 = 全部启用
	snap := s.SnapshotAll()
	if len(snap) != kindCount {
		t.Fatalf("快照 %d 项", len(snap))
	}
	for _, sn := range snap {
		if !sn.Enabled {
			t.Fatalf("%s 默认应启用", sn.Kind)
		}
		if sn.NextFire == "" {
			t.Fatalf("%s 启用态 next_fire 不应为空", sn.Kind)
		}
		if ts, err := time.Parse(time.RFC3339, sn.NextFire); err != nil || !ts.After(time.Now().Add(-time.Second)) {
			t.Fatalf("%s next_fire=%q 不是未来 RFC3339（err=%v）", sn.Kind, sn.NextFire, err)
		}
		if sn.LastRunUnix != 0 || sn.LastResult != "" || sn.Running {
			t.Fatalf("%s 初始观测字段应为空： %+v", sn.Kind, sn)
		}
	}
	if snap[0].Label != "签到" || len(snap[0].Hours) == 0 {
		t.Fatalf("中文标签/小时表缺失：%+v", snap[0])
	}
}

func TestSetEnabledHotToggle(t *testing.T) {
	s := newTestSched(t, Config{CheckinDisabled: true})
	snap := s.SnapshotAll()
	if snap[0].Enabled || snap[0].NextFire != "" {
		t.Fatalf("禁用态快照错误：%+v", snap[0])
	}
	if !s.SetEnabled("checkin", true) {
		t.Fatal("SetEnabled 合法 kind 应返回 true")
	}
	if s.SetEnabled("nope", true) {
		t.Fatal("SetEnabled 非法 kind 应返回 false")
	}
	after := s.SnapshotAll()[0]
	if !after.Enabled || after.NextFire == "" {
		t.Fatalf("热启用未生效：%+v", after)
	}
}

// TestRunWakesOnEnabledTransition 覆盖 Run 主循环两处 wake 分支：
// 全禁用停在"等通知"分支时 SetEnabled 能使其继续运转；ctx 取消能终止。
func TestRunWakesOnEnabledTransition(t *testing.T) {
	s := newTestSched(t, Config{
		CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	if !s.SetEnabled("keepalive", true) {
		t.Fatal("SetEnabled keepalive 失败")
	}
	// 唤醒后 Run 重算并阻塞在 timer 上；取消 ctx 应立即退出（证明没卡死在旧分支）。
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wake→重算→ctx 取消路径未让 Run 退出")
	}
}

func TestRunKindNowBusyAndErrors(t *testing.T) {
	s := newTestSched(t, Config{})
	if err := s.RunKindNow("nope"); err == nil {
		t.Fatal("非法 kind 应报错")
	}
	// 白盒占锁模拟撞车（空池真跑是瞬时的，无法竞态复现 busy）。
	s.runMu[taskCheckin].Lock()
	if err := s.RunKindNow("checkin"); !errors.Is(err, ErrBusy) {
		s.runMu[taskCheckin].Unlock()
		t.Fatalf("撞车应回 ErrBusy，实得 %v", err)
	}
	s.runMu[taskCheckin].Unlock()
	// 正常路径：空池 checkin 瞬时完成并记录观测。
	if err := s.RunKindNow("checkin"); err != nil {
		t.Fatalf("RunKindNow(checkin): %v", err)
	}
	sn := s.SnapshotAll()[0]
	if sn.Running {
		t.Fatal("结束后 running 应复位")
	}
	if sn.LastRunUnix == 0 || !strings.Contains(sn.LastResult, "ok=0 already=0 fail=0 skipped=0") {
		t.Fatalf("空池 checkin 摘要异常：%+v", sn)
	}
}

func TestDispatchRecordsKeepaliveSummary(t *testing.T) {
	s := newTestSched(t, Config{})
	s.dispatch(context.Background(), taskKeepalive)
	var sn KindSnapshot
	for _, x := range s.SnapshotAll() {
		if x.Kind == "keepalive" {
			sn = x
		}
	}
	if sn.LastRunUnix == 0 || !strings.Contains(sn.LastResult, "done") {
		t.Fatalf("keepalive 观测未记录：%+v", sn)
	}
}
