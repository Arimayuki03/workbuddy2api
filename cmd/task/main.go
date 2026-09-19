// task.go — 定时任务手动一次性触发器（与常驻服务的自动排程互不影响）。
//
// 用法：
//
//	./task <kind>        # kind ∈ checkin | activity | keepalive | travel | school | cat | minichat | all
//
// 独立进程读取工作目录 config.json + auths/，构建 pool+upstream 后直接调用
// scheduler 的 RunXxxNow——立即执行一次与定时排程完全相同的任务体，不重启、
// 不触碰常驻服务的排程时钟（两边各跑各的；幂等性由上游端点与 scheduler 的
// 防抖/已签到/已领取判定兜底）。
//
// school/cat 经由 scheduler 内部 pythonCmd() 起 python 脚本：默认 "python3"，
// Windows 只有 "python" 时设环境变量 WB2A_PYTHON=python 覆盖。
//
// all 顺序：keepalive（先刷新 token，后续任务不吃过期凭证）→ checkin → travel
// → activity → school → cat → minichat。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/config"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// cfgFile 与 cmd/activity 同构：只取本工具需要的字段；Schedule 段用
// internal/config 的同一份结构 + 默认值，缺省不各自漂移。
type cfgFile struct {
	AuthDir   string          `json:"auth_dir"`
	StateFile string          `json:"state_file"`
	Schedule  config.Schedule `json:"schedule"`
	Upstream  struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`
	// Pool 只取 expiring_soon 一个字段（与 cmd/server config 的 pool.expiring_soon
	// 同名同义）：签到分桶查余额的快过期窗口，见 scheduler.Config.ExpiringSoonWindow。
	// 其余 pool 段字段（冷却/熔断/降权参数等）是常驻服务的选号运行态，一次性
	// 任务进程不消费。
	Pool struct {
		ExpiringSoon string `json:"expiring_soon"`
	} `json:"pool"`
}

// defaultExpiringSoon 快过期窗口默认值，与 cmd/server/config.go Default() 的
// pool.expiring_soon 同源同值（168h）：config 缺该字段时 task 与 server 行为一致。
const defaultExpiringSoon = "168h"

const usage = `用法: task <checkin|activity|keepalive|travel|school|cat|minichat|all>
  checkin   每日签到（已签到幂等）        activity  活跃上报（N 连发+领猫联动）
  keepalive 令牌保活（按需 refresh）      school    开学季任务（python 脚本）
  travel    猫猫旅行巡检                 cat       夜猫子任务（python 脚本）
  minichat  小程序成长任务（python 脚本） all       全部跑一遍（顺序见上，minichat 垫后）`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	kind := strings.ToLower(os.Args[1])
	switch kind {
	case "checkin", "activity", "keepalive", "travel", "school", "cat", "minichat", "all":
	default:
		fmt.Fprintf(os.Stderr, "未知任务 %q\n\n%s\n", kind, usage)
		os.Exit(2)
	}

	raw, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	c := cfgFile{Schedule: config.DefaultSchedule()}
	if err := json.Unmarshal(raw, &c); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if err := c.Schedule.Normalize(); err != nil {
		log.Fatalf("normalize schedule: %v", err)
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	if c.StateFile == "" {
		c.StateFile = "data/state.json"
	}

	// 快过期窗口接线（与 server 侧同构，语义对齐 cmd/server/config.go normalize）：
	// 空值回落默认 168h；显式 "0" = 禁用分桶；负值钳 0（避免 upstream 判定窗口反转）。
	// 漏接的后果：RunCheckinNow 经 UserResourceDetailed(a, 0) 分桶退化，SetCreditsDetailed
	// 把 creditsExpiring 桶清零，"快过期先用"权重（×8）归零，直到 server 下次定时签到才恢复。
	expiringSoon := defaultExpiringSoon
	if c.Pool.ExpiringSoon != "" {
		expiringSoon = c.Pool.ExpiringSoon
	}
	expiringSoonWindow, err := time.ParseDuration(expiringSoon)
	if err != nil {
		log.Fatalf("parse pool.expiring_soon: %v", err)
	}
	if expiringSoonWindow < 0 {
		expiringSoonWindow = 0
	}

	auths, err := auth.LoadDir(c.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("task %s: loaded %d account(s)", kind, len(auths))

	p := pool.New(c.StateFile)
	defer p.Close() // 进程退出前停后台落盘 goroutine + 最后补一次落盘（与 cmd/server 同约定）
	p.SyncToDir(auths)

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            newUpstream(&c),
		CheckinHours:        c.Schedule.CheckinHours,
		TravelHours:         c.Schedule.TravelHours,
		ActivityHours:       c.Schedule.ActivityHours,
		KeepaliveHours:      c.Schedule.KeepaliveHours,
		SchoolHours:         c.Schedule.SchoolHours,
		CatHours:            c.Schedule.CatHours,
		ActivityReportCount: c.Schedule.ActivityReportCount,
		ExpiringSoonWindow:  expiringSoonWindow, // 快过期积分优先消耗（issue:积分过期；与 cmd/server 同构）
	})

	if kind == "all" {
		for _, k := range []string{"keepalive", "checkin", "travel", "activity", "school", "cat", "minichat"} {
			run(sch, k)
		}
		log.Printf("task all: complete")
		return
	}
	run(sch, kind)
	log.Printf("task %s: complete", kind)
}

// run 分派到与定时排程同体的立即执行方法（global 账号门控在 scheduler 内部
// 统一处理：school/cat/activity/checkin 对 global 账号自动跳过，与排程口径一致）。
func run(sch *scheduler.Scheduler, kind string) {
	switch kind {
	case "checkin":
		sch.RunCheckinNow()
	case "activity":
		sch.RunActivityNow()
	case "keepalive":
		sch.RunKeepaliveNow()
	case "travel":
		sch.RunTravelNow()
	case "school":
		sch.RunSchoolNow()
	case "cat":
		sch.RunCatNow()
	case "minichat":
		sch.RunMinichatNow()
	}
}

// newUpstream 构造上游客户端并显式接线 global realm 路由（与 cmd/activity 同风格）。
func newUpstream(c *cfgFile) *upstream.Client {
	up := upstream.New()
	up.GlobalEnabled = true
	if c.Upstream.TimeoutSeconds > 0 {
		up.HTTP.Timeout = time.Duration(c.Upstream.TimeoutSeconds) * time.Second
	}
	return up
}
