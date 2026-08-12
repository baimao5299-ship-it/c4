// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/scheduler"
)

// 用量提取：缓存读取/写入 token 的跨协议归一化（仿 sub2api 的
// cache_read/cache_creation 计费语义）。缺失字段 = 0（不阻塞采集）。
// 拆分独立函数以便用真实上游 JSON 构造单测（评审 I-1：不得结构体
// marshal 自证）。

// cacheCreationFromRaw 从 usage 原始 JSON 聚合缓存写入 token：OpenAI 的
// cache_creation.ephemeral_5m/1h_input_tokens 两个 TTL 桶求和。SDK 不解析
// cache_creation 对象（v1.12.0 结构体无此字段），必须走 RawJSON() 的
// 上游原始字节（评审 I-1 方案）。
func cacheCreationFromRaw(raw string) int64 {
	return gjson.Get(raw, "cache_creation.ephemeral_5m_input_tokens").Int() +
		gjson.Get(raw, "cache_creation.ephemeral_1h_input_tokens").Int()
}

// chatStreamUsage 流式 chat usage 帧 → 元组（调用方已用
// usage.Type == gjson.JSON 判定非空，显式 null 帧不得清零）。
// cached_tokens 在 prompt_tokens_details 下（评审 I-1：与 SDK
// CompletionUsage 结构体一致——顶层无该字段，流式/非流式同构）。
func chatStreamUsage(data []byte) (it, ot, tt, cr, cc int64) {
	return gjson.GetBytes(data, "usage.prompt_tokens").Int(),
		gjson.GetBytes(data, "usage.completion_tokens").Int(),
		gjson.GetBytes(data, "usage.total_tokens").Int(),
		gjson.GetBytes(data, "usage.prompt_tokens_details.cached_tokens").Int(),
		gjson.GetBytes(data, "usage.cache_creation.ephemeral_5m_input_tokens").Int() +
			gjson.GetBytes(data, "usage.cache_creation.ephemeral_1h_input_tokens").Int()
}

// anthropicStartUsage 流式 message_start 帧的 message.usage → (input, cacheRead,
// cacheCreation)。Anthropic 流式用量在 message_start 的 message.usage 里
// （评审 M1：前缀 message.usage.*，非顶层）。
func anthropicStartUsage(data []byte) (it, cr, cc int64) {
	return gjson.GetBytes(data, "message.usage.input_tokens").Int(),
		gjson.GetBytes(data, "message.usage.cache_read_input_tokens").Int(),
		gjson.GetBytes(data, "message.usage.cache_creation_input_tokens").Int()
}

// anthropicDeltaOutput 流式 message_delta 帧的 usage.output_tokens
// （message_delta.usage 不含 input/cache 字段）。
func anthropicDeltaOutput(data []byte) int64 {
	return gjson.GetBytes(data, "usage.output_tokens").Int()
}

// responsesCompletedUsage 流式 response.completed 帧 → 元组（评审 M2：
// 前缀 response.usage.*；cr 在 input_tokens_details.cached_tokens；
// cc 走 ephemeral 聚合——Responses 无 cache_creation 对象，恒 0 预期）。
func responsesCompletedUsage(data []byte) (it, ot, tt, cr, cc int64) {
	return gjson.GetBytes(data, "response.usage.input_tokens").Int(),
		gjson.GetBytes(data, "response.usage.output_tokens").Int(),
		gjson.GetBytes(data, "response.usage.total_tokens").Int(),
		gjson.GetBytes(data, "response.usage.input_tokens_details.cached_tokens").Int(),
		gjson.GetBytes(data, "response.usage.cache_creation.ephemeral_5m_input_tokens").Int() +
			gjson.GetBytes(data, "response.usage.cache_creation.ephemeral_1h_input_tokens").Int()
}

// chatUsageFromResponse 非流式 chat 响应用量：cr 直读 SDK 结构体字段
// （PromptTokensDetails.CachedTokens，v1.12.0 有该字段）；cc 从 RawJSON()
// （SDK 保留的上游原始字节）gjson 聚合——SDK 不解析 cache_creation 对象
// （评审 I-1 方案）。调用方已用 resp.JSON.Usage.Valid() 防护。
func chatUsageFromResponse(u openai.CompletionUsage) (it, ot, tt, cr, cc int64) {
	return u.PromptTokens, u.CompletionTokens, u.TotalTokens,
		u.PromptTokensDetails.CachedTokens, cacheCreationFromRaw(u.RawJSON())
}

// responsesUsageFromResponse 非流式 Responses 响应用量：同 chat 的
// 直读 + RawJSON 方案（cc 恒 0 预期——M4）。
func responsesUsageFromResponse(u responses.ResponseUsage) (it, ot, tt, cr, cc int64) {
	return u.InputTokens, u.OutputTokens, u.InputTokens + u.OutputTokens,
		u.InputTokensDetails.CachedTokens, cacheCreationFromRaw(u.RawJSON())
}

// anthropicUsageFromResponse 非流式 Anthropic 响应用量：SDK v1.56.0 Usage
// 结构体直读（CacheRead/CacheCreationInputTokens 字段存在）。
func anthropicUsageFromResponse(u anthropic.Usage) (it, ot, tt, cr, cc int64) {
	return u.InputTokens, u.OutputTokens, u.InputTokens + u.OutputTokens,
		u.CacheReadInputTokens, u.CacheCreationInputTokens
}

// --- resp/resp-ws 响应侧 image 检测旁路（spec §6；检测开关判定 + 计数提取） ---

// respImageDetectOn 响应侧 image 检测开关判定（spec §6 检测矩阵，用户裁决）：
// api_key（默认）永不检测；responses-special / codex-oauth / codex-pat 按模板
// ext strip_image_tools 联动——开（默认）= 不检测（图像工具已剥，响应不会出
// 图——旁路多余）；关 = 检测。
//
// 分层标注（V1-V3 实证）：codex 类型（chatgpt.com 上游）图片生成 = 客户端
// 本地执行（image_generation_call 由客户端本地组装、上游不产出）→ 网关 resp
// 响应无图片 item → 检测恒 0 计数（旁路无效但无害，保留统一逻辑）；
// responses-special（官方上游 hosted 触发）才有计数对象。故本函数对 codex
// 类型照常返回 true——计数由上游响应内容自然归零，不在判定层特判。
func respImageDetectOn(sel *scheduler.Selection) bool {
	if sel.StripImageTools {
		return false
	}
	switch sel.CredentialType {
	case credential.TypeResponsesSpecial, credential.TypeCodexOAuth, credential.TypeCodexPAT:
		return true
	}
	return false
}

// respImageCount 族：数 response 对象的 image_generation_call item（spec §6）。
// 两种输入形态（输出定位路径不同）各一个入口，共享计数核心 countImageItems——
// 调用方按形态直选，每次调用恰好 1 次 gjson 扫描（不做"先试 A 再回退 B"的
// 双扫描）：
//   - respImageCountCompleted：completed 帧/事件（response.output 信封——
//     流式 Observer 与 resp-ws relay sniff 同族）
//   - respImageCountBody：非流式响应体（output 顶层，SDK RawJSON）
//
// 判定规则（两形态同构）：
//   - type == "image_generation_call" 且 result 非空——status 不参与（V1-V3
//     实证：chatgpt.com 终态 status="generating" 非 "completed"，status 枚举
//     不可靠；result 必填非空 = 图已生成）
//   - 其他工具 item（function_call/web_search_call 等）的 result 字段在
//     type 过滤后不参与
//   - 有 id 按 id 去重 / id 缺失按出现顺序全数计入（与 SDK 同一 wire 语义）
//
// 热路径（复用 sniffResponsesCompleted 同族 gjson 用法）：单遍 ForEach 零
// 大分配——id 去重走栈数组 [16]string（官方 n 上限 10，零分配兜底；>16 唯一
// id 的病态帧去重退化为全数计数——只可能虚增不虚减，防御性接受）。无
// image item 的帧零分配（ForEach 空转）。
func respImageCountCompleted(data []byte) int64 {
	return countImageItems(gjson.GetBytes(data, "response.output"))
}

// respImageCountBody 非流式响应体形态（SDK RawJSON：无 response 信封）。
func respImageCountBody(data []byte) int64 {
	return countImageItems(gjson.GetBytes(data, "output"))
}

// countImageItems 计数核心（输出数组定位由调用方完成——两形态仅路径不同，
// 拆双入口省去 fallback 双扫描）。
func countImageItems(out gjson.Result) int64 {
	var (
		count int64
		ids   [16]string
		n     int
	)
	out.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "image_generation_call" {
			return true // type 过滤：其他工具 item 的 result 不参与
		}
		r := item.Get("result")
		if !r.Exists() || r.Type == gjson.Null {
			return true // result 缺失/null：图未生成
		}
		if r.IsArray() {
			if r.Get("#").Int() <= 0 {
				return true // 空数组：无结果
			}
		} else if r.String() == "" {
			return true // 空串/标量空值：无结果
		}
		if id := item.Get("id").String(); id != "" {
			for i := 0; i < n; i++ {
				if ids[i] == id {
					return true // 已计数（id 去重）
				}
			}
			if n < len(ids) {
				ids[n] = id
				n++
			}
		} // id 缺失：按出现顺序全数计入（不走去重）
		count++
		return true
	})
	return count
}
