// admin.go — /admin 本地管理 API（TrafficMonitor 插件 workbuddy2api-trafficmonitor-plugin 配套）。
//
// 安全边界（三道）：
//  1. config admin.enabled=true 才注册路由（缺省 false：整个二进制一条 /admin* 路由都不存在，
//     未启用部署的行为与引入前逐位一致）；
//  2. 仅接受 loopback（127.0.0.1/::1）来源——listen 是 0.0.0.0 时，shutdown/credits 这类
//     敏感操作也不泄漏到局域网，LAN 一律 403；
//  3. 复用既有 Bearer api_key 鉴权（api_key 为空 = 与 /status 一致的本地无鉴权语义）。
//
// 端点：
//
//	GET   /admin/tasks        任务快照：kind/中文标签/enabled/hours/next_fire/running/last_run/last_result
//	POST  /admin/tasks/run    {kind|"all"} 异步手动触发；同 kind 撞车回 409（不排队不重复打上游）
//	PATCH /admin/tasks        {kind, enabled} 热改排程开关 + 写回 config.json（最小 diff + .bak 备份）
//	POST  /admin/credits      实时积分（每号 1 次上游余额查询）：服务端冷却 + 单飞，超频 429
//	GET   /admin/credits      上次查询缓存 + 冷却截止时间（纯本地，零上游）
//	PATCH /admin/credits-interval {interval_sec} 热改冷却 + 写回 config.json（60–86400 秒）
//	POST  /admin/shutdown     优雅停机：走 main 注入的 ctx cancel（flush state → 关 store → srv.Shutdown）
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"workbuddy2api/internal/scheduler"
)

// AdminConfig /admin 端点配置（由 cmd/server 从 config.json 注入）。
type AdminConfig struct {
	// Enabled 总开关（config admin.enabled，缺省 false = 不注册任何 /admin 路由）。
	Enabled bool
	// CreditRefreshMinInterval POST /admin/credits 两次上游查询的最小间隔（防手滑连点打爆上游，
	// 触发腾讯侧风控）。<=0 兜底 600s。计时锚点是查询开始时刻：慢查询自然拉长间隔。
	CreditRefreshMinInterval time.Duration
	// ConfigPath PATCH /admin/tasks 写回用的 config.json 路径（main 的 -config 实参，
	// 相对路径按服务进程 CWD 解析）。空 = 只改内存不落盘（响应 persisted=false 注明）。
	ConfigPath string
}

// adminState /admin 运行态。挂在 Handler.adm，仅 Admin.Enabled 时非 nil。
type adminState struct {
	// mu 除保护下列运行态字段外，还保护 h.cfg.Admin.CreditRefreshMinInterval 的
	// 热改读写：该字段挂在共享的 cfg 上，PATCH /admin/credits-interval 写、
	// GET/POST /admin/credits 读，锁外访问即数据竞争。registerAdmin 里的启动期
	// 默认值写入先于任何请求（无并发），不在其列。
	mu sync.Mutex
	// manual 手动触发去重标记（kind → true）：让 /tasks/run 能在响应里如实报告 busy，
	// 而不是把撞车甩给调度器的 ErrBusy 日志。实际互斥仍由 scheduler.runMu 保证。
	manual map[string]bool
	// creditRunning/creditLastStart 积分查询单飞 + 冷却（锚点=开始时刻）。
	creditRunning   bool
	creditLastStart time.Time
	// creditCache 上次成功查询的完整回执（含逐号 ok/error），GET /admin/credits 直接回放。
	creditCache *creditReport
}

// creditAccount/creditTotal/creditReport 与 cmd/credit 的 JSON 契约逐字段一致
// （uid/nickname/remain/used/size/packages/ok/error + total 汇总），插件两端一套解析。
type creditAccount struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	Packages int    `json:"packages,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

type creditTotal struct {
	Remain   int64 `json:"remain"`
	Used     int64 `json:"used"`
	Size     int64 `json:"size"`
	Accounts int   `json:"accounts"`
	OK       int   `json:"ok"`
	Failed   int   `json:"failed"`
}

type creditReport struct {
	Service  string          `json:"service"`
	Ts       int64           `json:"ts"`
	Total    creditTotal     `json:"total"`
	Accounts []creditAccount `json:"accounts"`
}

// registerAdmin 注册全部 /admin 路由（NewHandler 在 cfg.Admin.Enabled 时调用）。
func (h *Handler) registerAdmin() {
	h.adm = &adminState{manual: map[string]bool{}}
	// 启动期写默认值：先于任何请求，无并发；此后该字段的热改读写一律走 adm.mu。
	if h.cfg.Admin.CreditRefreshMinInterval <= 0 {
		h.cfg.Admin.CreditRefreshMinInterval = 600 * time.Second
	}
	h.mux.HandleFunc("GET /admin/tasks", h.withAdmin(h.adminTasks))
	h.mux.HandleFunc("POST /admin/tasks/run", h.withAdmin(h.adminTaskRun))
	h.mux.HandleFunc("PATCH /admin/tasks", h.withAdmin(h.adminTaskPatch))
	h.mux.HandleFunc("GET /admin/credits", h.withAdmin(h.adminCreditsGet))
	h.mux.HandleFunc("POST /admin/credits", h.withAdmin(h.adminCreditsRefresh))
	h.mux.HandleFunc("PATCH /admin/credits-interval", h.withAdmin(h.adminCreditsIntervalPatch))
	h.mux.HandleFunc("POST /admin/shutdown", h.withAdmin(h.adminShutdown))
	log.Printf("admin API 已启用（loopback only，%d 个端点，积分查询冷却 %s）",
		7, h.cfg.Admin.CreditRefreshMinInterval)
}

// withAdmin 在既有 Bearer 鉴权前再垫一道 loopback 闸。
func (h *Handler) withAdmin(next http.HandlerFunc) http.HandlerFunc {
	authed := h.withAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackAddr(r.RemoteAddr) {
			writeOpenAIError(w, http.StatusForbidden, "forbidden", "/admin 仅接受本机（loopback）请求")
			return
		}
		authed(w, r)
	}
}

// isLoopbackAddr RemoteAddr（ip:port 或带 zone 的 IPv6）→ 是否回环来源。
func isLoopbackAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// decodeAdminJSON 解析小体积管理请求体（4KB 上限、拒未知字段——typo 直接 400，
// 免得 PATCH 少个字母把"没改"当成"改好了"）。成功返回 true。
func decodeAdminJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体 JSON 不合法: "+err.Error())
		return false
	}
	return true
}

func admin503(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"service": ServiceName,
		"error":   "scheduler not wired",
	})
}

// adminTasks 六类任务快照（含禁用项；next_fire 对禁用项为空串）。
func (h *Handler) adminTasks(w http.ResponseWriter, _ *http.Request) {
	if h.cfg.Sched == nil {
		admin503(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": ServiceName,
		"tasks":   h.cfg.Sched.SnapshotAll(),
	})
}

// adminTaskRun 手动触发（异步语义）：起 goroutine 跑 scheduler.RunKindNow，
// 202 立即返回——旅行/活跃全量遍历可达分钟级，不吊着 HTTP 连接，进度经
// GET /admin/tasks 的 running/last_run/last_result 观测。撞车 409（不落队）。
func (h *Handler) adminTaskRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
	}
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	if h.cfg.Sched == nil {
		admin503(w)
		return
	}
	targets := scheduler.Kinds()
	if req.Kind != "all" {
		if _, ok := scheduler.KindFromName(req.Kind); !ok {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("kind=%q 非法（可选：%v 或 all）", req.Kind, targets))
			return
		}
		targets = []string{req.Kind}
	}

	st := h.adm
	st.mu.Lock()
	var started, busy []string
	for _, k := range targets {
		if st.manual[k] {
			busy = append(busy, k)
			continue
		}
		st.manual[k] = true
		started = append(started, k)
	}
	st.mu.Unlock()

	if len(started) == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"service": ServiceName,
			"error":   "task_busy",
			"busy":    busy,
		})
		return
	}
	for _, k := range started {
		kind := k // 闭包捕获快照，防循环变量复用（Go 1.22+ 语义下冗余但无害）
		go func() {
			defer func() {
				st.mu.Lock()
				delete(st.manual, kind)
				st.mu.Unlock()
			}()
			log.Printf("admin: 手动触发 %s 开始", kind)
			if err := h.cfg.Sched.RunKindNow(kind); err != nil {
				// ErrBusy 只剩"定时器抢占窗口"一种可能（manual 标记已挡重发）：只记日志。
				if errors.Is(err, scheduler.ErrBusy) {
					log.Printf("admin: 手动触发 %s 撞车跳过（调度器执行中）: %v", kind, err)
					return
				}
				log.Printf("admin: 手动触发 %s 异常: %v", kind, err)
			}
		}()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"service": ServiceName,
		"started": started,
		"busy":    busy,
	})
}

// adminTaskPatch 热改排程开关：内存生效（调度器即时重排）+ 写回 config.json。
// 写回失败不回滚内存改动，但响应里 persisted=false + note 说明，重启后会退回旧值——
// 插件 UI 据此提示"仅本次运行生效"。
func (h *Handler) adminTaskPatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind    string `json:"kind"`
		Enabled bool   `json:"enabled"`
	}
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	if h.cfg.Sched == nil {
		admin503(w)
		return
	}
	if _, ok := scheduler.KindFromName(req.Kind); !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("kind=%q 非法（可选：%v）", req.Kind, scheduler.Kinds()))
		return
	}
	h.cfg.Sched.SetEnabled(req.Kind, req.Enabled)

	resp := map[string]any{
		"service":   ServiceName,
		"kind":      req.Kind,
		"enabled":   req.Enabled,
		"persisted": false,
	}
	path := h.cfg.Admin.ConfigPath
	if path == "" {
		resp["note"] = "ConfigPath 未配置，开关仅内存生效（重启后丢失）"
	} else {
		changed, err := patchConfigBool(path, "schedule", req.Kind+"_enabled", req.Enabled)
		switch {
		case err != nil:
			resp["note"] = "写回 config.json 失败，开关仅内存生效（重启后丢失）: " + err.Error()
			log.Printf("WARN: admin: 写回 config.json: %v", err)
		case !changed:
			resp["persisted"] = true
			resp["note"] = "config.json 已是目标值，未改动"
		default:
			resp["persisted"] = true
			log.Printf("admin: schedule.%s_enabled=%v 已热生效并写回 %s（原文件备份 .bak）", req.Kind, req.Enabled, path)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminCreditsGet 回放上次查询缓存 + 冷却截止时间（零上游成本）。
func (h *Handler) adminCreditsGet(w http.ResponseWriter, _ *http.Request) {
	st := h.adm
	st.mu.Lock()
	// interval 与下方运行态同锁读：PATCH /admin/credits-interval 可在别的
	// goroutine 热改它（字段挂在共享 cfg 上，锁外读即数据竞争）。
	interval := h.cfg.Admin.CreditRefreshMinInterval
	cache := st.creditCache
	var next int64
	if !st.creditLastStart.IsZero() {
		next = st.creditLastStart.Add(interval).Unix()
	}
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, creditsView(cache, next))
}

// adminCreditsIntervalPatch 热改积分查询冷却：内存立即生效（含已在跑的冷却，
// 下次 GET/POST /admin/credits 即按新间隔算）+ 写回 config.json（重启后保持）。
// TrafficMonitor 插件保存"自动刷新周期"时调用——插件按服务端冷却节奏查询，
// 冷却不同步的话插件侧周期再小也会被 429 顶回，形同虚设。
func (h *Handler) adminCreditsIntervalPatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IntervalSec int64 `json:"interval_sec"`
	}
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	// 风控兜底闸：下限 60s。再小就逼近上游风控阈值了，宁可直接拒绝也不放开。
	if req.IntervalSec < 60 || req.IntervalSec > 86400 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("interval_sec=%d 超出范围（60–86400 秒）", req.IntervalSec))
		return
	}
	// 内存热改纳入 adm.mu：GET/POST /admin/credits 在别的 goroutine 锁读该字段，
	// 裸写即数据竞争。锁一放新间隔即时生效（下次 GET/POST 即按新值算冷却）。
	st := h.adm
	st.mu.Lock()
	h.cfg.Admin.CreditRefreshMinInterval = time.Duration(req.IntervalSec) * time.Second
	st.mu.Unlock()

	resp := map[string]any{
		"service":      ServiceName,
		"interval_sec": req.IntervalSec,
		"persisted":    false,
	}
	path := h.cfg.Admin.ConfigPath
	if path == "" {
		resp["note"] = "ConfigPath 未配置，冷却仅内存生效（重启后丢失）"
	} else {
		changed, err := patchConfigInt(path, "admin", "credit_refresh_min_interval_sec", req.IntervalSec)
		switch {
		case err != nil:
			resp["note"] = "写回 config.json 失败，冷却仅内存生效（重启后丢失）: " + err.Error()
			log.Printf("WARN: admin: 写回 config.json: %v", err)
		case !changed:
			resp["persisted"] = true
			resp["note"] = "config.json 已是目标值，未改动"
		default:
			resp["persisted"] = true
			log.Printf("admin: admin.credit_refresh_min_interval_sec=%d 已热生效并写回 %s（原文件备份 .bak）",
				req.IntervalSec, path)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// creditsView 统一组装积分查询响应体：无缓存只回 cached=false + cooldown_until。
func creditsView(cache *creditReport, cooldownUntil int64) map[string]any {
	v := map[string]any{
		"service":        ServiceName,
		"cached":         cache != nil,
		"cooldown_until": cooldownUntil,
	}
	if cache != nil {
		v["credit_service"] = cache.Service
		v["ts"] = cache.Ts
		v["total"] = cache.Total
		v["accounts"] = cache.Accounts
	}
	return v
}

// adminCreditsRefresh 实时积分查询：逐号 ResourceSummary（与 cmd/credit 同口径、
// 同 200ms 账号间隔限速），成功的账号把权威余额回写 ledger（/status 立即变准）。
// 服务端两道保护：单飞（并发 429 running）+ 最小间隔冷却（429 + Retry-After）。
// 逐号串行全程感知 r.Context()：轮首检查 + 账号间限速等待均可被客户端断开打断，
// 提前收尾时响应带 aborted/completed/note（部分回执不覆盖 creditCache——其契约
// 是"上次成功查询的完整回执"）。单号 ResourceSummary 自身不感知 ctx（120s 硬
// 超时，client.go），断开后最多再等当前账号返回。
// 这是全链路唯一打上游余额接口的入口，频控以服务端为准——插件/UI 只是第一道装饰。
func (h *Handler) adminCreditsRefresh(w http.ResponseWriter, r *http.Request) {
	st := h.adm
	st.mu.Lock()
	// interval 同 adminCreditsGet：锁内读，防与热改 PATCH 构成数据竞争。
	interval := h.cfg.Admin.CreditRefreshMinInterval
	if st.creditRunning {
		st.mu.Unlock()
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"service": ServiceName, "error": "credit_refresh_running",
			"retry_after_sec": int(interval.Seconds()),
		})
		return
	}
	if !st.creditLastStart.IsZero() {
		if rem := time.Until(st.creditLastStart.Add(interval)); rem > 0 {
			st.mu.Unlock()
			w.Header().Set("Retry-After", strconv.Itoa(int(rem.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"service": ServiceName, "error": "credit_refresh_cooldown",
				"retry_after_sec": int(rem.Seconds()) + 1,
				"cooldown_until":  st.creditLastStart.Add(interval).Unix(),
			})
			return
		}
	}
	st.creditRunning = true
	st.creditLastStart = time.Now()
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		st.creditRunning = false
		st.mu.Unlock()
	}()

	statuses := h.cfg.Pool.List()
	rep := &creditReport{Service: "workbuddy", Ts: time.Now().Unix()}
	rep.Accounts = make([]creditAccount, 0, len(statuses))
	// aborted 客户端在串行刷新中途断开：不再对剩余账号发起上游查询。
	aborted := false
	for i, s := range statuses {
		if r.Context().Err() != nil {
			aborted = true
			break
		}
		ca := creditAccount{UID: s.UID, Nickname: s.Nickname}
		a := h.cfg.Pool.AuthByUID(s.UID)
		if a == nil || a.AccessTokenValue() == "" {
			ca.Error = "no credentials"
		} else {
			remain, used, size, packs, err := h.cfg.Upstream.ResourceSummary(a)
			if err != nil {
				ca.Error = err.Error()
			} else {
				ca.Remain, ca.Used, ca.Size = &remain, &used, &size
				ca.Packages, ca.OK = packs, true
				// 权威余额回写：估算账本校正 + 有余额即解冻（与 CheckinAll 收尾同两板斧）。
				h.cfg.Pool.ReenableIfCredits(s.UID, remain)
				h.cfg.Pool.SetCredits(s.UID, remain)
			}
		}
		rep.Accounts = append(rep.Accounts, ca)
		if ca.OK {
			rep.Total.OK++
			if ca.Remain != nil {
				rep.Total.Remain += *ca.Remain
				rep.Total.Used += *ca.Used
				rep.Total.Size += *ca.Size
			}
		}
		if i < len(statuses)-1 {
			// 与 cmd/credit collect 同口径限速；等待期间客户端断开则提前收尾，
			// 不再白等 N×200ms（更不再进入下一号可达分钟级的上游查询）。
			select {
			case <-time.After(200 * time.Millisecond):
			case <-r.Context().Done():
				aborted = true
			}
			if aborted {
				break
			}
		}
	}
	rep.Total.Accounts = len(rep.Accounts)
	rep.Total.Failed = rep.Total.Accounts - rep.Total.OK
	if aborted {
		log.Printf("admin: 积分实时查询中止（客户端断开）：已完成 %d/%d，ok=%d",
			rep.Total.Accounts, len(statuses), rep.Total.OK)
	} else {
		log.Printf("admin: 积分实时查询完成 ok=%d/%d（remain=%d）", rep.Total.OK, rep.Total.Accounts, rep.Total.Remain)
	}

	st.mu.Lock()
	if !aborted {
		st.creditCache = rep // 部分回执不入缓存：creditCache 契约是"上次完整回执"
	}
	next := st.creditLastStart.Add(interval).Unix()
	st.mu.Unlock()

	resp := creditsView(rep, next)
	if aborted {
		resp["aborted"] = true
		resp["completed"] = rep.Total.Accounts
		resp["total_accounts"] = len(statuses)
		resp["note"] = "客户端断开，提前终止"
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminShutdown 优雅停机：先回 200（插件要拿到成功回执再进入"等待端口释放"轮询），
// 300ms 后触发 main 注入的 ctx cancel，走既有关机路径：p.Flush 落盘 → store.Close
// → srv.Shutdown → ListenAndServe 正常返回。OnShutdown 未接线时退化为 os.Exit(0)。
func (h *Handler) adminShutdown(w http.ResponseWriter, _ *http.Request) {
	log.Printf("admin: 收到优雅停机请求（POST /admin/shutdown）")
	writeJSON(w, http.StatusOK, map[string]any{"service": ServiceName, "ok": true, "message": "shutting down"})
	cb := h.cfg.OnShutdown
	go func() {
		time.Sleep(300 * time.Millisecond)
		if cb != nil {
			cb()
		} else {
			os.Exit(0)
		}
	}()
}

// ============================================================================
// config.json 最小 diff 标量补丁（PATCH /admin/tasks、PATCH /admin/credits-interval 落盘用）
// ============================================================================

// patchConfigBool 把配置文件里二级对象 section.key 的布尔值原子替换（或插入）。
func patchConfigBool(path, section, key string, val bool) (bool, error) {
	lit := []byte("false")
	if val {
		lit = []byte("true")
	}
	return patchConfigScalar(path, section, key, lit)
}

// patchConfigInt 同 patchConfigBool，但写整数标量（splice 对任意标量字面量通用）。
func patchConfigInt(path, section, key string, val int64) (bool, error) {
	return patchConfigScalar(path, section, key, []byte(strconv.FormatInt(val, 10)))
}

// patchConfigMu 串行化 config.json 的读-改-写临界区。PATCH /admin/tasks 与
// PATCH /admin/credits-interval 都经 patchConfigScalar 落盘：无互斥时并发 PATCH
// 各自基于旧字节做 splice，后落盘者覆盖先落盘者的改动（补丁丢失）而两个响应仍都
// 报 persisted=true。adminState.mu 只管运行态字段，覆盖不到落盘路径，故设包级锁。
var patchConfigMu sync.Mutex

// patchConfigScalar 把配置文件里二级对象 section.key 的标量值原子替换（或插入），
// 其余字节原样保留——对用户的 config.json 是影响最小化：只有目标那一处变。
//
// 规则：
//   - 文件不是合法 JSON / 顶层不是对象 → 报错并拒绝写回（config.json 坏了服务下次
//     重启也起不来，不能在这里把现场覆盖掉）；
//   - key 存在 → 只替换其 value token 跨度；原值与目标相等时 changed=false 零写入；
//   - section 存在而 key 缺失 → 在 section 开括号后插入（缩进跟随现有第一个成员；
//     空对象则不带逗号）；
//   - section 整体缺失 → 在顶层末尾新增整个 section 对象；
//   - 写盘前新字节必须过 json.Valid；先备份原文件到 path+".bak"，再 tmp+rename
//     原子替换（与 state.json/auths 落盘同风格）。
//
// 返回 changed：文件字节是否发生改动（等值提交时 false）。
func patchConfigScalar(path, section, key string, lit []byte) (bool, error) {
	// 整个读-改-写持包级锁（含备份与 tmp+rename 落盘）：临界区必须覆盖 ReadFile
	// 到 Rename 的完整跨度，否则并发 PATCH 仍是"后写者基于旧字节"。
	patchConfigMu.Lock()
	defer patchConfigMu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}
	newRaw, changed, err := spliceBool(raw, section, key, lit)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if !json.Valid(newRaw) {
		return false, errors.New("internal error: 补丁后的 JSON 非法，已放弃写入")
	}
	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		return false, fmt.Errorf("backup: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return false, fmt.Errorf("open tmp: %w", err)
	}
	if _, err := f.Write(newRaw); err != nil {
		f.Close()
		os.Remove(tmp)
		return false, fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("rename: %w", err)
	}
	return true, nil
}

// spliceBool 用 json.Decoder token 流定位 section.key 的标量值并做跨度替换/插入
// （名字沿用旧称）。只依赖 Token()+InputOffset()，不重建对象——键序、未知字段、
// 数字/字符串原文全部原样；lit 是目标标量的 JSON 字面量（true/false/数字/字符串）。
func spliceBool(raw []byte, section, key string, lit []byte) ([]byte, bool, error) {
	if !json.Valid(raw) {
		return nil, false, fmt.Errorf("config 不是合法 JSON，拒绝写回")
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false, fmt.Errorf("config 顶层不是 JSON 对象，拒绝写回")
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		obj     bool
		key     string // 本 frame（作为父对象成员时）的键；顶层 frame 为空
		wantKey bool
	}
	var stack []frame
	var topOpen, secOpen int64 = -1, -1 // 顶层 / section 对象 '{' 之后的偏移
	pending := false                    // 已读到目标成员名，下一个标量 token 是其值
	pendingPre := int64(0)

	for {
		t, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("扫描 config 失败: %w", err)
		}
		if d, ok := t.(json.Delim); ok {
			if pending {
				// 目标 key 的值不是标量（对象/数组）——不敢猜，直接拒写。
				return nil, false, fmt.Errorf("config 里 %s.%s 的值不是标量，拒绝写回", section, key)
			}
			switch d {
			case '{', '[':
				end := dec.InputOffset()
				stack = append(stack, frame{obj: d == '{', wantKey: d == '{'})
				if len(stack) == 1 && d == '{' {
					topOpen = end
				} else if len(stack) == 2 && d == '{' && secOpen < 0 && stack[0].key == section {
					secOpen = end
				}
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].obj {
					stack[len(stack)-1].wantKey = true
				}
			}
			continue
		}
		// 标量 token
		if len(stack) == 0 {
			return nil, false, fmt.Errorf("config 顶层不是 JSON 对象，拒绝写回")
		}
		f := &stack[len(stack)-1]
		off := dec.InputOffset()
		if f.wantKey {
			s, _ := t.(string) // 对象成员名必为字符串
			f.key = s
			f.wantKey = false
			if len(stack) == 2 && stack[0].key == section && s == key {
				pending, pendingPre = true, off
			}
			continue
		}
		if pending {
			// 值跨度：[vStart, off)。Decoder 不为键值间的 `:` 出 token，
			// 需从名字 token 后依次越过：空白 → `:` → 空白，才到布尔字面量首字节。
			skipWS := func(i int64) int64 {
				for i < off && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n') {
					i++
				}
				return i
			}
			vStart := skipWS(pendingPre)
			if vStart >= off || raw[vStart] != ':' {
				return nil, false, fmt.Errorf("config 结构异常：找不到 %s.%s 的值分隔符，拒绝写回", section, key)
			}
			vStart = skipWS(vStart + 1)
			if bytes.Equal(raw[vStart:off], lit) {
				return raw, false, nil // 等值：零改动
			}
			out := make([]byte, 0, len(raw)+8)
			out = append(out, raw[:vStart]...)
			out = append(out, lit...)
			out = append(out, raw[off:]...)
			return out, true, nil
		}
		f.wantKey = f.obj
	}

	// —— 未命中值：走插入路径 ——
	ins := []byte("\"" + key + "\": " + string(lit))
	if secOpen >= 0 { // section 存在，key 缺失
		indent := memberIndentAfter(raw, secOpen)
		needComma := firstNonSpaceByte(raw, secOpen) != '}'
		var add []byte
		add = append(add, '\n')
		add = append(add, indent...)
		add = append(add, ins...)
		if needComma {
			add = append(add, ',')
		}
		out := make([]byte, 0, len(raw)+len(add))
		out = append(out, raw[:secOpen]...)
		out = append(out, add...)
		out = append(out, raw[secOpen:]...)
		return out, true, nil
	}
	// section 整体缺失：在顶层对象末尾新增（成员缩进 4 空格、闭合缩进 2，与主流
	// config.json 风格对齐；这是"新文件从未有 schedule 段"的少见路径，只求可读不求精美）。
	secBody := []byte("\"" + section + "\": {" + "\n    " + string(ins) + "\n  }")
	last := len(raw)
	for last > 0 && (raw[last-1] == ' ' || raw[last-1] == '\t' || raw[last-1] == '\r' || raw[last-1] == '\n') {
		last--
	}
	if last == 0 || raw[last-1] != '}' || topOpen < 0 {
		return nil, false, fmt.Errorf("config 结构不符合预期（找不到顶层对象收尾），拒绝写回")
	}
	emptyTop := firstNonSpaceByte(raw, topOpen) == '}'
	var add []byte
	if emptyTop {
		add = append([]byte("\n  "), secBody...)
		add = append(add, '\n')
		insertAt := topOpen
		out := make([]byte, 0, len(raw)+len(add))
		out = append(out, raw[:insertAt]...)
		out = append(out, add...)
		out = append(out, raw[last-1:]...) // 保留原顶层收尾 '}'（及其后空白）
		return out, true, nil
	}
	// 非空顶层：逗号贴在最后一个成员值之后（越过其后的空白），保持惯用排版。
	cut := last - 1
	for cut > 0 && (raw[cut-1] == ' ' || raw[cut-1] == '\t' || raw[cut-1] == '\r' || raw[cut-1] == '\n') {
		cut--
	}
	out := make([]byte, 0, len(raw)+len(secBody)+8)
	out = append(out, raw[:cut]...)
	out = append(out, ",\n  "...)
	out = append(out, secBody...)
	out = append(out, raw[cut:]...) // 原有的收尾空白 + '}'
	return out, true, nil
}

// firstNonSpaceByte 从 off 起第一个非空白字节的值（越界返回 0）。
func firstNonSpaceByte(raw []byte, off int64) byte {
	for int64(len(raw)) > off {
		if c := raw[off]; c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			return c
		}
		off++
	}
	return 0
}

// memberIndentAfter 对象开括号后第一个成员行的缩进前缀（含起始换行后的空白）。
// 找不到成员行（空对象）或无换行缩进时回落两个空格。
func memberIndentAfter(raw []byte, openOff int64) []byte {
	i := openOff
	for i < int64(len(raw)) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n') {
		i++
	}
	if i >= int64(len(raw)) || raw[i] != '"' {
		return []byte("  ")
	}
	lineStart := i
	for lineStart > 0 && raw[lineStart-1] != '\n' {
		lineStart--
	}
	ind := raw[lineStart:i]
	if len(ind) == 0 {
		return []byte("  ")
	}
	return ind
}
