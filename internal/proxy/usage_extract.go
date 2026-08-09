package proxy

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
	"github.com/tidwall/gjson"
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
