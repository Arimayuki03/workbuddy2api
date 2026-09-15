// task.go — 定时任务手动一次性触发器（与常驻服务的自动排程互不影响）。
//
// 用法：
//
//	./task <kind>        # kind ∈ checkin | activity | keepalive | travel | school | cat | all
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
// → activity → school → cat。
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
}

const usage = `用法: task <checkin|activity|keepalive|travel|school|cat|all>
  checkin   每日签到（已签到幂等）        activity  活跃上报（N 连发+领猫联动）
  keepalive 令牌保活（按需 refresh）      school    开学季任务（python 脚本）
  travel    猫猫旅行巡检                 cat       夜猫子任务（python 脚本）
  all       全部跑一遍（keepalive→checkin→travel→activity→school→cat）`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	kind := strings.ToLower(os.Args[1])
	switch kind {
	case "checkin", "activity", "keepalive", "travel", "school", "cat", "all":
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

	auths, err := auth.LoadDir(c.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("task %s: loaded %d account(s)", kind, len(auths))

	p := pool.New(c.StateFile)
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
	})

	if kind == "all" {
		for _, k := range []string{"keepalive", "checkin", "travel", "activity", "school", "cat"} {
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
