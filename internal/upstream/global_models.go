// global 模型名目录探测：只产模型名，不产倍率（PLAN §3.D2「模型名目录 ≠ 倍率表」）。
//
// credits 数值一律不进入本包实现——探测端点即便返回倍率字段也忽略，名单只喂
// /v1/models 的 global: 前缀输出，不注入 costTier、不参与选号。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

// GlobalModelNames 国际版（global realm）模型名静态名单兜底（PLAN §7.2 附录 21 名）。
// 只含模型名、不含倍率。探测失败 / 无 global 账号时直接输出此名单；
// 探测成功时以其 "权威 21 名" 为基底，追加探测独有的模型名（去重）。
var GlobalModelNames = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"hy4-preview",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"deep-model",
	"deepseek-v4.1-flash",
	"gpt-6-astra",
	"hy4-preview-f",
	"hy3",
	"glm-5.2",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gemini-3.5-flash",
	"glm-5.3",
	"kimi-k3",
	"kimi-k2.6",
}

// fetchGlobalModelsCache 探测结果缓存（语义参照 CN 侧 dynamicModelsCache：1h TTL +
// 5min 失败负缓存）。按 Client 实例持有（effortsMu 同模式），测试新建 Client 即隔离。
type fetchGlobalModelsCache struct {
	sync.Mutex
	names     []string // 成功缓存：探测 ∪ 静态名单（已去重）；nil = 未探测
	fetched   time.Time
	lastFail  time.Time
}

// globalModelsTTL / globalModelsFailCooldown 探测缓存时长：成功 1h，失败 5min 负缓存。
const (
	globalModelsTTL        = time.Hour
	globalModelsFailCooldown = 5 * time.Minute
)

// globalModelsProbePaths global 模型目录端点候选序列（按 realm 切 base，路径"家族"）：
// console 家族优先（与 global chat 路径同域），404/500 时换 /v2（CLI 窄表）。
// 参考 PLAN v1 §2.2 分歧③：console 路径在 global 上或 500，/v2 返回窄表。
var globalModelsProbePaths = []string{
	"/console/enterprises/personal/models",
	"/v2/enterprises/personal/models",
}

// FetchGlobalModels 探测 global 账号的模型名目录并返回模型名列表（含 context 无关、无倍率）。
//
// 成功：探测结果 ∪ GlobalModelNames（去重，静态 21 为基底，探测独有追加），缓存 1h。
// 失败（家族端点全非 2xx / 解析失败 / 空列表）：记 5min 负缓存，回落 GlobalModelNames。
// 缓存命中（成功缓存未过期 → 直接返回；负缓存冷却期内 → 直接返回静态名单）零上游调用。
//
// 调用方负责传递 global realm 账号（realm=global）与判定"有无 global 账号"（无则不应调本方法）。
func (c *Client) FetchGlobalModels(a *auth.Auth) []string {
	if cached := c.globalModelCacheHit(); cached != nil {
		return cached
	}

	names, err := c.probeGlobalModels(a)
	if err != nil || len(names) == 0 {
		// 探测失败：负缓存 + 回落静态名单。
		c.globalModelsMu.Lock()
		c.globalModels.lastFail = time.Now()
		c.globalModels.names = nil
		c.globalModelsMu.Unlock()
		return GlobalModelNames
	}

	// 成功：静态名单为基底，追加探测独有（去重）。只取名字，倍率字段忽略。
	seen := make(map[string]bool, len(GlobalModelNames)+len(names))
	merged := make([]string, 0, len(GlobalModelNames)+len(names))
	for _, id := range GlobalModelNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}
	for _, id := range names {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}

	c.globalModelsMu.Lock()
	c.globalModels.names = merged
	c.globalModels.fetched = time.Now()
	c.globalModels.lastFail = time.Time{}
	c.globalModelsMu.Unlock()
	return merged
}

// globalModelCacheHit 返回缓存中的名单：成功缓存未过期 → 返回缓存；负缓存冷却期内 → nil
// 的等价（调用方当回落静态）。未命中返回 nil（无成功缓存且不在负缓存冷却期）。
func (c *Client) globalModelCacheHit() []string {
	c.globalModelsMu.Lock()
	defer c.globalModelsMu.Unlock()
	if len(c.globalModels.names) > 0 && time.Since(c.globalModels.fetched) < globalModelsTTL {
		return c.globalModels.names
	}
	if !c.globalModels.lastFail.IsZero() && time.Since(c.globalModels.lastFail) < globalModelsFailCooldown {
		// 负缓存冷却期内：避免反复打上游，直接按失败处理（回落静态）。
		return GlobalModelNames
	}
	return nil
}

// probeGlobalModels 按候选路径序列发起一次探测，返回模型名列表（未去重、已滤 disabled）。
// 家族端点全部非 2xx（等幂探活）才返回错误。
func (c *Client) probeGlobalModels(a *auth.Auth) ([]string, error) {
	var lastErr error
	for _, path := range globalModelsProbePaths {
		names, err := c.globalModelsOnce(a, path)
		if err != nil {
			lastErr = err
			continue
		}
		return names, nil
	}
	return nil, lastErr
}

// globalModelsOnce 单端点探测。2xx + 解析出非空名单 → (names, nil)；否则 (nil, err)。
func (c *Client) globalModelsOnce(a *auth.Auth, path string) ([]string, error) {
	url := c.chatBase(a) + path // 按 realm 切 base：global 账号 → global base
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 共享请求头（Origin/Referer/UA），与 FetchModels 同款
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("global models status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	return parseGlobalModelNames(raw)
}

// parseGlobalModelNames 容忍两种形态解析模型名：
//   - 对象数组：data.models[].id/.name（id 优先），disabled 剔除；
//   - 窄表：data 为字符串数组。
//
// 解析成功但名单为空 → 返回错误（调用方回落静态，等价"该端点没给全"）。
func parseGlobalModelNames(raw []byte) ([]string, error) {
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("global models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("global models code=%d", env.Code)
	}
	trimmed := strings.TrimSpace(string(env.Data))
	if strings.HasPrefix(trimmed, "[") {
		// 窄表形态：data 为字符串数组。
		var arr []string
		if err := json.Unmarshal(env.Data, &arr); err != nil {
			return nil, fmt.Errorf("global models parse (narrow): %w", err)
		}
		out := make([]string, 0, len(arr))
		for _, id := range arr {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("global models empty list")
		}
		return out, nil
	}
	// 对象形态：data.models[].id/.name（id 优先），disabled 剔除。
	var obj struct {
		Models []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Disabled bool   `json:"disabled"`
		} `json:"models"`
	}
	if err := json.Unmarshal(env.Data, &obj); err != nil {
		return nil, fmt.Errorf("global models parse: %w", err)
	}
	out := make([]string, 0, len(obj.Models))
	for _, m := range obj.Models {
		id := m.ID
		if id == "" {
			id = m.Name
		}
		if id == "" || m.Disabled {
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("global models empty list")
	}
	return out, nil
}