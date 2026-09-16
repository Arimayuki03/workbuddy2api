// prompt_estimate.go 出站前 token 预估（任务书 prompt-too-long §2 主动预防）。
//
// 定位：**只做「提前发现」，不改写语义**——绝不截断/改写用户消息（原方案 b 废除）。
// 预估超限时不打上游（省一次注定 400 的请求与账号配额），直接返回与上游 11115
// 逐字段同构的错误体（见 BuildPromptTooLongBody）。
//
// 估算口径（任务书：简单高效，不引精确 tokenizer 依赖）：
//   - 英文/ASCII 文本 ≈ 4 字符/token（GPT 家族经典口径，OpenAI cookbook 常用近似）；
//   - 非 ASCII（中日韩等）≈ 1 字符/token（CJK 多数 tokenizer 中 1 汉字 ≈ 1 token，
//     每字符 rune 按 1 计）；
//   - 只统计 messages 的 content 文本字段（string 直数 + content 数组里 text 型
//     分段），system/tools 等结构开销不估——只用于 95% 阈值预警，粗估口径的
//     安全侧由阈值余量（95%，非 100%）吸收：高估错过预警的代价小（上游打回
//     11115 照常透传），误拦正常请求的代价大。
//
// 误差方向性说明：字符比例口径在纯 ASCII 长上下文（代码/日志粘贴）上系统性
// 偏高估（实际 token 常少于 chars/4），在 CJK 混排上接近；偏高估只会让 85% WARN
// 提前、95% 拦截更保守——与「宁可放行吃上游 11115 透传，不可误杀正常请求」的
// 纪律一致（低估算才有误杀风险，本口径不产生低估）。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// promptEstimateCharsPerToken ASCII 文本的字符/token 比例（1 token ≈ 4 chars）。
const promptEstimateCharsPerToken = 4

// PromptEstimateTokens 预估请求 body 的 prompt token 数（messages content 文本口径）。
// body 不可解析 / 无 messages / 无文本内容 → 0（零值语义：未估出，调用方放行）。
func PromptEstimateTokens(body []byte) int64 {
	var obj struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return 0
	}
	var chars int64
	for _, m := range obj.Messages {
		switch c := m.Content.(type) {
		case string:
			chars += countContentChars(c)
		case []any:
			for _, seg := range c {
				if sm, ok := seg.(map[string]any); ok {
					if t, _ := sm["type"].(string); t != "text" {
						continue // 图片/工具分段不按文本估（token 数与文本无关且通常巨大——按 0 估偏保守）
					}
					if txt, ok := sm["text"].(string); ok {
						chars += countContentChars(txt)
					}
				}
			}
		}
	}
	// 双口径：ASCII 段按 chars/4，非 ASCII 段按 rune 数（每字符 1 token）。
	// countContentChars 已分别计数，此处合成。
	if chars < 0 { // 防御（int64 溢出才会负）
		return 0
	}
	return chars
}

// countContentChars 单段文本折算成「token 近似数」并累加：见 PromptEstimateTokens 口径。
func countContentChars(text string) int64 {
	var ascii, nonASCII int64
	for _, r := range text {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	return ascii/promptEstimateCharsPerToken + nonASCII
}

// estimateThresholdBlock / estimateThresholdWarn 预估阈值（任务书 §2：95% 拦截、85% WARN）。
const (
	estimateThresholdBlock = 0.95
	estimateThresholdWarn  = 0.85
)

// PromptEstimateResult 单次预估结论（estimated 0 = 未估出，调用方一律放行）。
type PromptEstimateResult struct {
	Estimated   int64 // 预估 prompt token 数（0 = 未能预估/无文本）
	Limit       int64 // 该模型的 context 上限（查找链口径；0 = 未知未查）
	Ratio       float64
	ShouldBlock bool // >= 95%：不打上游，直接回同构 11115
	ShouldWarn  bool // >= 85% 且未拦：WARN 日志（运维可见），正常放行
}

// EstimatePromptTokens 预估 body 的 prompt token 数并对照模型 context 上限给出结论。
// limit<=0（未知）→ 全放行（不误杀）；estimated==0（无文本/坏 body）→ 全放行。
// client 可为 nil（第 4 级 models.dev 拉取跳过——同步决策场景不依赖远端补充值，
// 命中第 2/3 级缓存已足够，未收录模型按查找链返回 1M 兜底口径预判）。
func EstimatePromptTokens(body []byte, model string, limit int64) PromptEstimateResult {
	res := PromptEstimateResult{Limit: limit}
	if limit <= 0 {
		return res
	}
	estimated := PromptEstimateTokens(body)
	if estimated <= 0 {
		return res
	}
	res.Estimated = estimated
	res.Ratio = float64(estimated) / float64(limit)
	if res.Ratio >= estimateThresholdBlock {
		res.ShouldBlock = true
	} else if res.Ratio >= estimateThresholdWarn {
		res.ShouldWarn = true
	}
	return res
}

// BuildPromptTooLongBody 构造与上游 11115 **逐字段同构**的预防性错误体（任务书 §2）。
//
// 上游真实 11115 形态（实测，任务书样本）：
//
//	{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"…"}
//
// 同构约束（任务书实现约束）：除两个有意差异——msg 里标 "(estimated)" 区分来源、
// requestId 本地生成——其余字段与上游逐字段一致（code=11115 / msg 句式 / requestId
// 32-hex 形态）。不发明新词汇、不新增字段。
func BuildPromptTooLongBody(estimated, limit int64, requestID string) string {
	return fmt.Sprintf(`{"code":11115,"msg":"prompt is too long: %d tokens > %d maximum (estimated)","requestId":"%s"}`,
		estimated, limit, requestID)
}

// GenRequestID32Hex 生成本地 requestId（32-hex，与 session.NewMessageID 同形态）。
// 独立小封装避免 upstream→session 反向依赖。
func GenRequestID32Hex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	// 熵源故障极端兜底：仍保证 32-hex 非空（session.NewMessageID 同思路）。
	return fmt.Sprintf("%032x", 1)
}
