// join.go — login join：一步登录流程（移植自本地 Windows 定制版，适配 realm 化结构）。
//
//	login [--realm=cn|global] join
//
// 替代 login.sh 的 bash+python 编排：拿 state+authUrl → 打开浏览器 → 自动轮询
// auth/token → 拉账号 →（仅 CN）每日签到 → 原子落盘 auths/workbuddy-<uid>.json。
// global realm 不签到（与 scheduler 的 global 门控口径一致）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	auth2 "workbuddy2api/internal/auth"
)

// billingBaseCN CN 计费域（签到端点所在）；global realm 不签到，无需 global 计费域。
const billingBaseCN = "https://www.codebuddy.cn"

// authDirDefault 登录产物落盘目录（与网关 config.auth_dir 缺省一致）；WB2A_AUTH_DIR 可覆盖。
const authDirDefault = "auths"

// openBrowser 用系统默认浏览器打开 url（Windows 用 rundll32；其他平台静默——
// 桌面环境差异大，交给用户手动复制链接）。
func openBrowser(url string) {
	if runtime.GOOS != "windows" {
		return
	}
	if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start(); err != nil {
		fmt.Printf("（自动打开浏览器失败：%v，请手动复制上方链接打开）\n", err)
	}
}

// runJoin 一步完成登录并落盘。base/origin/realm 由 main 按 --realm 解析后传入，
// 复用与 url/poll 完全相同的端点与请求头族。
func runJoin(base, origin, realm string, client *http.Client) {
	headers := commonHeaders(origin)

	// 1. 拿 state + authUrl
	data, _, err := doJSON(client, http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		fatal("auth state failed: %v", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		fatal("auth state: missing state or authUrl")
	}

	fmt.Println("请在浏览器完成登录：")
	fmt.Println()
	fmt.Println("  " + st.AuthURL)
	fmt.Println()
	openBrowser(st.AuthURL)

	// 2. 轮询 auth/token，直到 code=0 拿到 token（与 runPoll 相同的权威端点）
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	// 可打断退出：用户按任意键（含直接回车）即中止轮询，不登录不落盘。
	stop := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		for {
			if n, _ := os.Stdin.Read(buf); n > 0 {
				close(stop)
				return
			}
		}
	}()
	fmt.Println("等待授权... 请在浏览器中登录并确认（按任意键可中止，回到菜单）。")
	deadline := time.Now().Add(5 * time.Minute)
	aborted := false
pollLoop:
	for {
		select {
		case <-stop:
			aborted = true
			break pollLoop
		default:
		}
		tokRaw, _, errTok := doJSON(client, http.MethodGet, base+"/v2/plugin/auth/token?state="+st.State, headers, nil)
		if errTok == nil {
			if err := json.Unmarshal(tokRaw, &tok); err == nil && tok.AccessToken != "" {
				break // 登录完成
			}
		}
		if time.Now().After(deadline) {
			fatal("登录超时（5 分钟）。请重新运行 join。")
		}
		time.Sleep(3 * time.Second)
	}
	if aborted {
		fmt.Println("已中止，未保存任何账号。")
		// 非零退出码：让调用方（启动服务.bat）区分"已中止"与"成功"
		exitFunc(2)
	}

	// 3. 拉账号信息（uid/nickname/enterpriseId）
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		headers(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(client, http.MethodGet, base+"/v2/plugin/login/account?state="+st.State, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	if acct.UID == "" {
		acct.UID = st.State // 兜底：拿不到 uid 时用 state 占位
	}

	// 4. 每日签到（仅 CN；global 与 scheduler 门控口径一致跳过）。失败仅提示，不中断。
	if realm == realmCN {
		checkinBilling(acct.UID, acct.EnterpriseID, tok.AccessToken, tok.Domain)
	} else {
		fmt.Println("签到：global 账号跳过（国际版无每日签到）")
	}

	// 5. 落盘 auths/workbuddy-<uid>.json。组装嵌套形 doc 后走 auth.Parse + SaveAtomic：
	// 与网关读取/refresh 写回完全同一条持久化路径（tmp+rename、空凭证拒绝）。
	dir := authDirDefault
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		dir = v
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir %s: %v", dir, err)
	}
	normalizedRealm := auth2.ResolveRealm(realm, tok.Domain)
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  tok.AccessToken,
			"refreshToken": tok.RefreshToken,
			"expiresAt":    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
			"domain":       tok.Domain,
			"realm":        normalizedRealm,
		},
		"account": map[string]any{
			"uid":          acct.UID,
			"enterpriseId": acct.EnterpriseID,
			"nickname":     acct.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fatal("marshal auth: %v", err)
	}
	a, err := auth2.Parse(raw)
	if err != nil {
		fatal("validate auth: %v", err)
	}
	fp := filepath.Join(dir, "workbuddy-"+acct.UID+".json")
	a.FilePath = fp
	if err := a.SaveAtomic(); err != nil {
		fatal("write auth: %v", err)
	}

	fmt.Println()
	fmt.Println("登录成功！")
	fmt.Println("  UID      : " + acct.UID)
	fmt.Println("  Nickname : " + acct.Nickname)
	fmt.Println("  Realm    : " + normalizedRealm)
	fmt.Println("  文件     : " + fp)
	fmt.Println("  提示     : 若服务运行中，需重启才能加载新账号。")
}

// checkinBilling CN 每日签到（billing 域）。失败仅提示，不影响登录结果。
func checkinBilling(uid, entID, token, domain string) {
	req, err := http.NewRequest(http.MethodPost, billingBaseCN+"/v2/billing/meter/daily-checkin", bytes.NewReader([]byte("{}")))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	if entID != "" {
		req.Header.Set("X-Enterprise-Id", entID)
		req.Header.Set("X-Tenant-Id", entID)
	}
	if domain != "" {
		req.Header.Set("X-Domain", domain)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("签到失败：%v（不影响登录）\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var env apiEnvelope
	if json.Unmarshal(body, &env) == nil && env.Code == 0 {
		fmt.Println("签到：成功")
	} else {
		msg := env.Msg
		if msg == "" {
			msg = fmt.Sprintf("http %d", resp.StatusCode)
		}
		fmt.Printf("签到：%s（已签到或非关键，忽略）\n", msg)
	}
}
