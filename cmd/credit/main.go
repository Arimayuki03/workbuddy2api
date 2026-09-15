// credit.go — WorkBuddy 积分查询（全部账号 + 总计），JSON 输出到 stdout。
//
// 用法:
//
//	go run ./cmd/credit        # 或编译后 ./credit
//
// 输出结构:
//
//	{"service":"workbuddy","ts":N,
//	 "total":{"remain":N,"used":N,"size":N,"accounts":N,"ok":N,"failed":N},
//	 "accounts":[{"uid","nickname","remain","used","size","packages","ok","error?"}]}
//
// realm 感知：复用 upstream.Client（auth.Parse + upstream.New），global 账号查积分
// 走 workbuddy.ai /billing/meter/*（404 回落 /v2），CN 账号维持 codebuddy.cn
// /v2/billing/meter/get-user-resource（现状逐字）。聚合口径即 upstream.ResourceSummary。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

type accountResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	Packages int    `json:"packages,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	up := upstream.New()
	up.GlobalEnabled = true // 允许按 realm 路由：global 账查积分走 workbuddy.ai
	accounts := collect(authDir, up)
	printAccounts(accounts, pretty)
}

// collect 遍历 auths 目录并查询每个账号的积分摘要。供测试注入 fake upstream 断言
// realm 路由（main 从 os.Args/env 取况，collect 单一来源可测）。
// 文件清单走 auth.LoadAuthFiles（宽侧 workbuddy*.json）：与网关 LoadDir 同口径，
// 不带连字符的文件不再被跳过（P2-10，审查发现 10）。
func collect(authDir string, up *upstream.Client) []accountResult {
	files, _ := auth.LoadAuthFiles(authDir)
	accounts := make([]accountResult, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		res := accountResult{UID: a.UID, Nickname: a.Nickname}
		if a.AccessToken == "" {
			res.Error = "no accessToken"
			accounts = append(accounts, res)
			continue
		}
		remain, used, size, packs, err := up.ResourceSummary(a)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Remain = &remain
			res.Used = &used
			res.Size = &size
			res.Packages = packs
			res.OK = true
		}
		accounts = append(accounts, res)
		time.Sleep(200 * time.Millisecond)
	}
	return accounts
}

// printAccounts 汇总并输出结果：-pretty 走人类可读日报，否则 JSON（与老版输出一致）。
func printAccounts(accounts []accountResult, pretty bool) {
	var totalRemain, totalUsed, totalSize int64
	okCount := 0
	for _, a := range accounts {
		if a.OK {
			okCount++
			if a.Remain != nil {
				totalRemain += *a.Remain
			}
			if a.Used != nil {
				totalUsed += *a.Used
			}
			if a.Size != nil {
				totalSize += *a.Size
			}
		}
	}
	if pretty {
		printPretty(accounts, totalRemain, totalUsed, totalSize, okCount)
		return
	}
	out := map[string]any{
		"service": "workbuddy",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain":   totalRemain,
			"used":     totalUsed,
			"size":     totalSize,
			"accounts": len(accounts),
			"ok":       okCount,
			"failed":   len(accounts) - okCount,
		},
		"accounts": accounts,
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

// printPretty 人类可读日报：汇总一行 + 账号明细表（昵称/UID/剩余/已用/总量/状态）。
func printPretty(accounts []accountResult, totalRemain, totalUsed, totalSize int64, okCount int) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && a.Remain != nil && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, name+" "+a.Error)
		}
	}
	pct := int64(0)
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📊 WorkBuddy 积分日报\n")
	fmt.Fprintf(&b, "账号: %d/%d    总计: %d/%d    剩余: %d%%\n", withBalance, len(accounts), totalRemain, totalSize, pct)
	// 账号明细表（看用户 + 积分）
	fmt.Fprintf(&b, "%-14s %-14s %10s %10s %10s  %s\n", "昵称", "UID", "剩余", "已用", "总量", "状态")
	for _, a := range accounts {
		status := "✔"
		remain, used, size := "-", "-", "-"
		if a.OK && a.Remain != nil {
			remain, used, size = itoa(*a.Remain), itoa(*a.Used), itoa(*a.Size)
		}
		if !a.OK {
			status = "✘ " + trunc(a.Error, 24)
		}
		nick := a.Nickname
		if nick == "" {
			nick = "(无昵称)"
		}
		uid := a.UID
		if len(uid) > 14 {
			uid = uid[:14]
		}
		fmt.Fprintf(&b, "%-14s %-14s %10s %10s %10s  %s\n", trunc(nick, 14), uid, remain, used, size, status)
	}
	for _, f := range failed {
		fmt.Fprintf(&b, "⚠️ %s\n", f)
	}
	// 纯 UTF-8 输出。目标控制台按 UTF-8 渲染（实测：chcp 虽报 936，但 Windows Terminal/
	// UTF-8 控制台正确显示 UTF-8，GBK 反而乱码），因此不做任何代码页转换。
	fmt.Print(b.String())
}

// itoa 极简 int→string（避免只为输出格式化引 strconv）。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// trunc 截断字符串到 n 个字节（中文可能被截半，接受；仅用于表格对齐）。
func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}