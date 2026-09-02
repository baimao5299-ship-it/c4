// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"strconv"

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
	return addUsageTokens(
		gjson.Get(raw, "cache_creation.ephemeral_5m_input_tokens").Int(),
		gjson.Get(raw, "cache_creation.ephemeral_1h_input_tokens").Int(),
	)
}

// rawUsageNumber reads the first positive integer from a bounded list of
// provider spellings.  The SDK structs intentionally expose only the native
// protocol fields, while compatibility relays sometimes return the usage
// object from another protocol unchanged (for example input_tokens on a Chat
// response).  Keeping this fallback on the SDK's RawJSON preserves the exact
// response without a second full response unmarshal.
func rawUsageNumber(raw string, paths ...string) int64 {
	for _, path := range paths {
		value := gjson.Get(raw, path)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		n := value.Int()
		if n > 0 {
			return n
		}
	}
	return 0
}

// chatStreamUsage 流式 chat usage 帧 → 元组 + ok（usage 存在判定内建——调用方
// 不再前置 gjson 检查；显式 null 帧 ok=false 不清零：usageInterval 值首字节
// {/[ 判定，null 字面量首字节 n → 不存在）。
// cached_tokens 在 prompt_tokens_details 下（评审 I-1：与 SDK CompletionUsage
// 结构体一致——顶层无该字段，流式/非流式同构）。
func chatStreamUsage(data []byte) (usageTuple, bool) {
	raw, ok := usageInterval(data, usageKeyBytes)
	if !ok {
		return usageTuple{}, false
	}
	return usageFieldsFromInterval(raw, promptTokensKeyBytes, completionTokensKeyBytes, promptTokensDetailsKeyBytes), true
}

// chatStreamUsageEvent extracts usage from a Chat Completions stream frame,
// including relays that attach an SSE event name. OpenAI-compatible relays
// normally emit a data-only usage frame, but Claude adapters commonly keep the
// Anthropic event names while serving the OpenAI wire format. The old caller
// discarded every named frame before this parser could inspect it, which made
// successful Claude streams appear to have zero tokens and zero cost.
//
// A frame may carry either the OpenAI shape (usage.prompt_tokens /
// completion_tokens) or the Anthropic shape (message.usage / usage.output_tokens). The
// returned tuple is intentionally partial for Anthropic start/delta events;
// callers merge it with the values collected from the other event.
func chatStreamUsageEvent(eventName, data []byte) (usageTuple, bool) {
	// Some relays omit the SSE `event:` line and expose only the top-level
	// JSON `type`.  Normalize that here as well as in the relay package so all
	// callers (including direct Anthropic and conversion paths) share the same
	// behavior when they invoke this helper with a raw event name.
	eventName = usageStreamEventName(eventName, data)
	switch {
	case bytes.Equal(eventName, []byte("message_start")):
		// Standard Anthropic start frames nest usage under message. Only
		// select that shape when the message object is present; a relay may
		// reuse the event label for a normal OpenAI usage object.
		if t, ok := anthropicStartUsageCompat(data); ok {
			return t, true
		}
	case bytes.Equal(eventName, []byte("message_delta")):
		// Anthropic puts output_tokens under message_delta.usage. Keep the
		// presence check distinct from a zero value so an empty usage object
		// cannot be mistaken for a measured output count.
		if ot, ok := anthropicDeltaUsageCompat(data); ok {
			return usageTuple{ot: ot}, true
		}
	}
	// Claude-compatible relays may put the complete Anthropic summary in a
	// terminal/custom frame. Check that shape before the OpenAI parser: an
	// Anthropic object can also contain total_tokens, which would otherwise make
	// the native parser look non-empty while leaving input/output at zero.
	if summary, summaryOK := anthropicSummaryUsageCompat(data); summaryOK {
		return summary, true
	}
	// OpenAI-compatible usage is normally a complete top-level object and is
	// valid whether or not the relay attached an SSE event name.
	return chatStreamUsage(data)
}

// anthropicSummaryUsageCompat extracts a complete Anthropic usage object when
// a compatibility relay puts it on a terminal/custom SSE event instead of
// splitting it across message_start and message_delta. Anthropic cache reads
// are excluded from input_tokens and total_tokens, so this helper deliberately
// does not apply the OpenAI cache subtraction used by usageFieldsFromInterval.
func anthropicSummaryUsageCompat(data []byte) (usageTuple, bool) {
	raw, ok := usageInterval(data, usageKeyBytes)
	if !ok {
		return usageTuple{}, false
	}
	_, _, hasInput := scanKeyValue(raw, inputTokensKeyBytes)
	_, _, hasOutput := scanKeyValue(raw, outputTokensKeyBytes)
	_, _, hasCacheRead := scanKeyValue(raw, cacheReadInputTokensKeyBytes)
	_, _, hasCacheReadCompat := scanKeyValue(raw, cacheReadTokensKeyBytes)
	_, _, hasCacheCreate := scanKeyValue(raw, cacheCreationInputTokensKeyBytes)
	_, _, hasCacheWrite := scanKeyValue(raw, cacheWriteInputTokensKeyBytes)
	if !hasInput && !hasOutput && !hasCacheRead && !hasCacheReadCompat && !hasCacheCreate && !hasCacheWrite {
		return usageTuple{}, false
	}
	u := usageTuple{
		it: scanFieldInt64(raw, inputTokensKeyBytes),
		ot: scanFieldInt64(raw, outputTokensKeyBytes),
		cr: scanFieldInt64(raw, cacheReadInputTokensKeyBytes),
		cc: scanFieldInt64(raw, cacheCreationInputTokensKeyBytes),
		tt: scanFieldInt64(raw, totalTokensKeyBytes),
	}
	if u.it <= 0 {
		u.it = scanFieldInt64(raw, promptTokensKeyBytes)
	}
	if u.ot <= 0 {
		u.ot = scanFieldInt64(raw, completionTokensKeyBytes)
	}
	if u.cr <= 0 {
		u.cr = scanFieldInt64(raw, cacheReadTokensKeyBytes)
	}
	if u.cc <= 0 {
		u.cc = scanFieldInt64(raw, cacheWriteInputTokensKeyBytes)
	}
	if u.tt <= 0 {
		u.tt = addUsageTokens(u.it, u.ot)
	}
	return u, true
}

// usageStreamEventName returns the explicit SSE event name when present and
// otherwise reads a top-level JSON `type`.  The latter is needed for relays
// that emit data-only Anthropic/Responses frames.  Values are returned as
// slices into the current frame; callers must consume them synchronously.
func usageStreamEventName(eventName, data []byte) []byte {
	if len(eventName) > 0 {
		eventName = bytes.TrimSpace(eventName)
		if len(eventName) > 0 {
			return eventName
		}
	}
	start, end, ok := scanKeyValue(data, typeKeyBytes)
	if !ok || end <= start+1 || start >= len(data) || end > len(data) || data[start] != '"' || data[end-1] != '"' {
		return nil
	}
	return data[start+1 : end-1]
}

// mergeStreamUsage preserves fields collected from split protocol events while
// merging a later OpenAI usage frame field by field. Empty usage objects never
// erase a non-empty measurement.
func mergeStreamUsage(dst *usageTuple, next usageTuple) {
	if dst == nil {
		return
	}
	// A named frame can still be sparse (some relays emit Anthropic split
	// frames followed by an OpenAI-shaped summary). Merge measured fields
	// independently so a later sparse frame cannot erase input/cache usage.
	if next.it == 0 && next.ot == 0 && next.tt == 0 && next.cr == 0 && next.cc == 0 {
		return
	}
	if next.it > 0 {
		dst.it = next.it
	}
	if next.ot > 0 {
		dst.ot = next.ot
	}
	if next.tt > 0 {
		dst.tt = next.tt
	}
	if next.cr > 0 {
		dst.cr = next.cr
	}
	if next.cc > 0 {
		dst.cc = next.cc
	}
}

// anthropicStartUsage 流式 message_start 帧的 message.usage → 元组 + ok（input/
// cacheRead/cacheCreation；ot/tt 无对应字段恒 0——调用点下游 tt = it + ot 自算）。
// Anthropic 流式用量在 message_start 的 message.usage 里（评审 M1：前缀
// message.usage.*，非顶层）。显式 null / 缺失 → ok=false。
func anthropicStartUsage(data []byte) (usageTuple, bool) {
	start, end, ok := scanKeyValue(data, messageKeyBytes)
	if !ok {
		return usageTuple{}, false
	}
	raw, ok := usageInterval(data[start:end], usageKeyBytes)
	if !ok {
		return usageTuple{}, false
	}
	return usageTuple{
		it: scanFieldInt64(raw, inputTokensKeyBytes),
		cr: scanFieldInt64(raw, cacheReadInputTokensKeyBytes),
		cc: scanFieldInt64(raw, cacheCreationInputTokensKeyBytes),
	}, true
}

// anthropicStartUsageCompat accepts both the canonical Messages envelope
// (message.usage) and relays that flatten usage at the event's top level.
func anthropicStartUsageCompat(data []byte) (usageTuple, bool) {
	if t, ok := anthropicStartUsage(data); ok {
		return t, true
	}
	raw, ok := usageInterval(data, usageKeyBytes)
	if !ok {
		return usageTuple{}, false
	}
	t := usageTuple{
		it: scanFieldInt64(raw, inputTokensKeyBytes),
		cr: scanFieldInt64(raw, cacheReadInputTokensKeyBytes),
		cc: scanFieldInt64(raw, cacheCreationInputTokensKeyBytes),
	}
	if t.it <= 0 {
		t.it = scanFieldInt64(raw, promptTokensKeyBytes)
	}
	if t.cr <= 0 {
		t.cr = scanFieldInt64(raw, cachedTokensKeyBytes)
	}
	if t.cc <= 0 {
		t.cc = scanFieldInt64(raw, cacheCreationInputTokensKeyBytes)
	}
	return t, true
}

// anthropicDeltaOutput 流式 message_delta 帧的 usage.output_tokens
// （message_delta.usage 不含 input/cache 字段）。单字段统一走字节扫描族
// （与其余提取同构，消除 gjson 依赖）；缺失/显式 null → 0。
func anthropicDeltaOutput(data []byte) int64 {
	value, _ := anthropicDeltaUsage(data)
	return value
}

// anthropicDeltaUsage returns output_tokens plus a presence bit. The bit
// distinguishes an empty/null usage object from a measured zero and lets
// stream callers preserve an already observed output count.
func anthropicDeltaUsage(data []byte) (int64, bool) {
	raw, ok := usageInterval(data, usageKeyBytes)
	if !ok {
		return 0, false
	}
	_, _, present := scanKeyValue(raw, outputTokensKeyBytes)
	if !present {
		return 0, false
	}
	return scanFieldInt64(raw, outputTokensKeyBytes), true
}

// anthropicDeltaUsageCompat accepts output_tokens and the common OpenAI
// completion_tokens spelling used by compatibility relays.
func anthropicDeltaUsageCompat(data []byte) (int64, bool) {
	if value, ok := anthropicDeltaUsage(data); ok {
		return value, true
	}
	raw, ok := usageInterval(data, usageKeyBytes)
	if !ok {
		return 0, false
	}
	_, _, present := scanKeyValue(raw, completionTokensKeyBytes)
	if !present {
		return 0, false
	}
	return scanFieldInt64(raw, completionTokensKeyBytes), true
}

// responsesCompletedUsage 流式 response.completed 帧 → 元组 + ok（评审 M2：
// 前缀 response.usage.*；cr 在 input_tokens_details.cached_tokens；cc 走
// ephemeral 聚合——Responses 无 cache_creation 对象，恒 0 预期）。显式 null /
// 缺失 → ok=false（调用方保留此前值）。
func responsesCompletedUsage(data []byte) (usageTuple, bool) {
	start, end, ok := scanKeyValue(data, responseKeyBytes)
	if !ok {
		return usageTuple{}, false
	}
	raw, ok := usageInterval(data[start:end], usageKeyBytes)
	if !ok {
		return usageTuple{}, false
	}
	return usageFieldsFromInterval(raw, inputTokensKeyBytes, outputTokensKeyBytes, inputTokensDetailsKeyBytes), true
}

// --- codex resp 顶层 usage 解析（P1-1——T6：SDK 路径的 usage 形状为顶层） ---
// codex SSE data 载荷 usage 在**顶层**（{"type":"response.completed","response":
// {id,object,status},"usage":{...}}——codex-sdk responses.go:90-93 顶层读取实证）；
// 合成体（codex-sdk responses.go:113-119 responsesComposite：id/object/status/
// output/usage）无 type 字段但 usage 同样顶层。既有 sniffResponsesCompleted
//（"type":"response.completed" 子串预筛 + response.usage.* 前缀——WS 帧形状）
// 对两路径均不适用（预筛命中但读 0 / 预筛恒不命中——静默归零）——本族是 WS
// 形状之外的独立路径族（流式 completed 帧 / 合成体共用同一顶层 helper）。

// responsesTopLevelUsage 顶层 usage → 元组 + ok（流式 completed 帧与合成体
// 共用）：input/output/total + input_tokens_details.cached_tokens +
// cache_creation ephemeral 双桶聚合（与 cacheCreationFromRaw 同口径）。
// 显式 null / 缺失 → ok=false。
func responsesTopLevelUsage(data []byte) (usageTuple, bool) {
	raw, ok := usageInterval(data, usageKeyBytes)
	if !ok {
		return usageTuple{}, false
	}
	return usageFieldsFromInterval(raw, inputTokensKeyBytes, outputTokensKeyBytes, inputTokensDetailsKeyBytes), true
}

// responsesStreamUsage accepts both response.completed shapes emitted by
// Responses-compatible relays: the canonical nested response.usage object and
// the Codex-style top-level usage object.
func responsesStreamUsage(data []byte) (usageTuple, bool) {
	nested, nestedOK := responsesCompletedUsage(data)
	top, topOK := responsesTopLevelUsage(data)
	if !nestedOK {
		return top, topOK
	}
	if !topOK {
		return nested, true
	}
	// A few relays include both objects but populate them incrementally. Keep
	// the canonical nested values and fill only fields absent from that object
	// using the top-level representation.
	nestedTotalExplicit := responseUsageHasExplicitTotal(data)
	topTotalExplicit := topLevelUsageHasExplicitTotal(data)
	if nested.it <= 0 {
		nested.it = top.it
	}
	if nested.ot <= 0 {
		nested.ot = top.ot
	}
	if !nestedTotalExplicit && topTotalExplicit {
		nested.tt = top.tt
	} else if !nestedTotalExplicit && !topTotalExplicit {
		nested.tt = addUsageTokens(nested.it, nested.ot)
	}
	if nested.cr <= 0 {
		nested.cr = top.cr
	}
	if nested.cc <= 0 {
		nested.cc = top.cc
	}
	return nested, true
}

func responseUsageHasExplicitTotal(data []byte) bool {
	start, end, ok := scanKeyValue(data, responseKeyBytes)
	if !ok {
		return false
	}
	raw, ok := usageInterval(data[start:end], usageKeyBytes)
	if !ok {
		return false
	}
	_, _, ok = scanKeyValue(raw, totalTokensKeyBytes)
	return ok
}

func topLevelUsageHasExplicitTotal(data []byte) bool {
	raw, ok := usageInterval(data, usageKeyBytes)
	if !ok {
		return false
	}
	_, _, ok = scanKeyValue(raw, totalTokensKeyBytes)
	return ok
}

// sniffResponsesCompletedTop 流式 fn 热路径嗅探（P1-1）：字节扫描 **type 精确判
// 定** "type"=="response.completed"（SDK 交付载荷无 event: 行——正文含该子串
// 的消息帧不冻结；WS 路径 bytes.Contains 预筛形状不适用）+ 顶层 usage 解析
// 内联（不再经 responsesTopLevelUsage 二次定位——type 检查与 usage 提取共用
// usageInterval/usageFieldsFromInterval 提取 helper）。ok 语义不变：type 命中
// 即 true（usage 缺失 → 零值元组——缺失 = 0，不阻塞采集）。
// 真实上游 response.completed 恒唯一（终态事件）——调用方取首个命中帧后跳过
// 后续解析（usage 只读一次；"最后帧覆盖"语义由终态唯一性等价保证）。
func sniffResponsesCompletedTop(data []byte) (usageTuple, bool) {
	if !isResponsesCompletedType(data) {
		return usageTuple{}, false
	}
	raw, usageOK := usageInterval(data, usageKeyBytes)
	if !usageOK {
		return usageTuple{}, true // type 命中但 usage 缺失 → 零值元组（同旧行为）
	}
	return usageFieldsFromInterval(raw, inputTokensKeyBytes, outputTokensKeyBytes, inputTokensDetailsKeyBytes), true
}

// isResponsesCompletedType recognizes the terminal Responses event without
// relying on one exact byte spelling. Relays commonly insert whitespace after
// the colon or reorder top-level keys; a literal bytes.Contains check would
// miss those frames and make a successful request appear to use zero tokens.
// The comparison remains byte-exact for the value so text that merely mentions
// response.completed is never treated as a terminal event.
func isResponsesCompletedType(data []byte) bool {
	start, end, ok := scanKeyValue(data, typeKeyBytes)
	if !ok || end <= start+1 || start >= len(data) || end > len(data) || data[start] != '"' || data[end-1] != '"' {
		return false
	}
	return bytes.Equal(data[start+1:end-1], completedTypeBytes)
}

func isResponsesCompletedEvent(eventName, data []byte) bool {
	return bytes.Equal(eventName, completedTypeBytes) || isResponsesCompletedType(data)
}

// --- 字节扫描 helper（spec 2026-08-15-gc-opt-ab A-1：gjson 多遍扫描 →
// scanKeyValue 单遍字节扫描；热路径零分配——AllocsPerRun==0 测试钉住） ---

// usageInterval 定位指定键的 usage 值区间：scanKeyValue 单遍定位 + 存在性判定
// （值首字节 { 或 [ ——对齐 gjson JSON Type 含对象/数组；缺失与显式 null 字面量
// 首字节 n → 不存在）。调用方据 ok 决定是否更新元组——"显式 null 帧不得清零"
// 语义由该判定保证（与旧 gjson Type==JSON 前置检查等价）。
func usageInterval(data []byte, usageKey []byte) ([]byte, bool) {
	start, end, ok := scanKeyValue(data, usageKey)
	if !ok {
		return nil, false
	}
	raw := data[start:end]
	if len(raw) == 0 || raw[0] != '{' && raw[0] != '[' {
		return nil, false
	}
	return raw, true
}

// deductCacheRead OpenAI 族输入归一（spec 2026-08-25-input-cache-billing-normalization）：
// OpenAI 语义 cached ⊆ input，InputTokens 承载**可计费输入**（扣除缓存读；cr
// 单独按 CacheReadPerM 计价——否则缓存部分被 input 价与 cache-read 价重复计费）。
// Anthropic 语义 cache_read ∉ input_tokens，不经本函数。病态上游 cr > it → 钳 0
// 防负车道（cr 原样保留）。cr 缺失/为 0 → 恒等。
func deductCacheRead(it, cr int64) int64 {
	if cr <= 0 {
		return it
	}
	if cr >= it {
		return 0
	}
	return it - cr
}

// usageFieldsFromInterval 从 usage 值区间提取五计数元组（chat/responses 两协议
// 共用——字段名按协议经参数注入：chat 为 prompt_tokens/completion_tokens/
// prompt_tokens_details.cached_tokens，responses 为 input_tokens/output_tokens/
// input_tokens_details.cached_tokens；cache_creation ephemeral 双桶聚合两协议
// 同构）。crKey 恒非 nil（调用方按协议注入其 cached_tokens 内嵌路径——评审
// 认定 nil 分支死代码；Anthropic 不经本 helper，其 cr 由 anthropicStartUsage
// 直读 cache_read_input_tokens）。键名不匹配/缺失 → 0（与 gjson 缺失 = 0 等价）。
// 出口施加 deductCacheRead 归一——it 为可计费输入（spec 2026-08-25）；tt 保持
// 线上原值（数值不变量：归一只迁移组成，不改 total——配额扣减按 TotalTokens，
// 数值恒等）。
func usageFieldsFromInterval(raw []byte, itKey, otKey, crKey []byte) usageTuple {
	var u usageTuple
	u.it = scanFieldInt64(raw, itKey)
	u.ot = scanFieldInt64(raw, otKey)
	u.tt = scanFieldInt64(raw, totalTokensKeyBytes)
	// A few OpenAI-compatible relays omit total_tokens even though the two
	// component counters are present. Preserve the provider total when valid,
	// but derive a saturated fallback when it is missing or non-positive so a
	// successful request cannot become free merely because of a sparse usage
	// object. Use the raw input count before cache normalization: cached tokens
	// are part of the provider total even when billed separately.
	if u.tt <= 0 {
		u.tt = addUsageTokens(u.it, u.ot)
	}
	if s, e, ok := scanKeyValue(raw, crKey); ok {
		u.cr = scanFieldInt64(raw[s:e], cachedTokensKeyBytes)
	}
	if s, e, ok := scanKeyValue(raw, cacheCreationKeyBytes); ok {
		u.cc = addUsageTokens(scanFieldInt64(raw[s:e], ephemeral5mKeyBytes), scanFieldInt64(raw[s:e], ephemeral1hKeyBytes))
	}
	u.it = deductCacheRead(u.it, u.cr)
	return u
}

// scanFieldInt64 定位键值区间并解析 int64（缺失 → 0）。
func scanFieldInt64(raw []byte, key []byte) int64 {
	s, e, ok := scanKeyValue(raw, key)
	if !ok {
		return 0
	}
	return scanIntValue(raw[s:e])
}

// scanIntValue 值区间 → int64（usage 计数字段专用）。首字节 " 则
// parseJSONString 剥引号再 ParseInt（gjson Int() 对字符串数字同解析——gjson.go
// String 分支 parseInt）；数字字面直解；null/缺失 → 0（ParseInt 报错路径兜底）。
// string([]byte) 转换走编译器免分配优化路径（实测 AllocsPerRun 0——spec A-1）。
//
// 与 gjson 的病态差异（均为"保守 0"方向，注释标注——usage 字段恒整数 token
// 计数，实际影响趋零）：
//   - float 字面（12.5）：gjson safeInt 截断取 12，本实现 ParseInt 报错 → 0
//   - 非纯十进制整数（小数点/指数 1e3/超 int64 回绕）：gjson parseInt 无溢出
//     检查，本实现 ParseInt 报错 → 0
//   - 字符串数字含 \uXXXX 转义（"12"）：gjson 值 unescape 后解析，本实现
//     parseJSONString 转义原样保留（同族惯例，对键从不 decodeUnicodeEscapes）
//     → ParseInt 失败 → 0
//   - bool 字面：gjson true → 1 / false → 0，本实现非数字字面 → 0
func scanIntValue(raw []byte) int64 {
	if len(raw) == 0 {
		return 0
	}
	if raw[0] == '"' {
		val, _ := parseJSONString(raw, 0)
		if val == nil {
			return 0 // 未闭合（json.Valid 前置下不可达，防御性兜底）
		}
		n, err := strconv.ParseInt(string(val), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// chatUsageFromResponse 非流式 chat 响应用量：cr 直读 SDK 结构体字段
// （PromptTokensDetails.CachedTokens，v1.12.0 有该字段）；cc 从 RawJSON()
// （SDK 保留的上游原始字节）gjson 聚合——SDK 不解析 cache_creation 对象
// （评审 I-1 方案）。调用方已用 resp.JSON.Usage.Valid() 防护。
// 出口施加 deductCacheRead 归一（spec 2026-08-25）——it 为可计费输入；tt 信任
// 上游 TotalTokens 原值（数值不变量：归一不改 total）。上游 total 与 in+out 的
// 既有分歧维持现状（不收敛也不扩大）。
func chatUsageFromResponse(u openai.CompletionUsage) (it, ot, tt, cr, cc int64) {
	raw := u.RawJSON()
	it, ot, tt, cr = u.PromptTokens, u.CompletionTokens, u.TotalTokens,
		u.PromptTokensDetails.CachedTokens
	// Some OpenAI-compatible relays return an Anthropic/Responses-shaped usage
	// object from the Chat route. The SDK keeps that object in RawJSON but leaves
	// the native fields at zero, which previously made a successful request free.
	if it <= 0 {
		it = rawUsageNumber(raw, "prompt_tokens", "input_tokens")
	}
	if ot <= 0 {
		ot = rawUsageNumber(raw, "completion_tokens", "output_tokens")
	}
	if tt <= 0 {
		tt = rawUsageNumber(raw, "total_tokens")
	}
	if cr <= 0 {
		cr = rawUsageNumber(raw, "prompt_tokens_details.cached_tokens", "cache_read_input_tokens", "cache_read_tokens", "cached_tokens")
	}
	cc = cacheCreationFromRaw(raw)
	if cc <= 0 {
		cc = rawUsageNumber(raw, "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation.input_tokens")
	}
	if tt <= 0 {
		tt = addUsageTokens(it, ot)
	}
	return deductCacheRead(it, cr), ot, tt, cr, cc
}

// responsesUsageFromResponse 非流式 Responses 响应用量：同 chat 的
// 直读 + RawJSON 方案（cc 恒 0 预期——M4）。出口施加 deductCacheRead 归一
// （spec 2026-08-25）——tt 先按原始 in+out 定值再归一 it（数值不变量：归一
// 不改 total）。
func responsesUsageFromResponse(u responses.ResponseUsage) (it, ot, tt, cr, cc int64) {
	raw := u.RawJSON()
	it, ot, tt, cr = u.InputTokens, u.OutputTokens, u.TotalTokens,
		u.InputTokensDetails.CachedTokens
	if it <= 0 {
		it = rawUsageNumber(raw, "input_tokens", "prompt_tokens")
	}
	if ot <= 0 {
		ot = rawUsageNumber(raw, "output_tokens", "completion_tokens")
	}
	if tt <= 0 {
		tt = rawUsageNumber(raw, "total_tokens")
	}
	if cr <= 0 {
		cr = rawUsageNumber(raw, "input_tokens_details.cached_tokens", "cache_read_input_tokens", "cache_read_tokens", "cached_tokens")
	}
	cc = cacheCreationFromRaw(raw)
	if cc <= 0 {
		cc = rawUsageNumber(raw, "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation.input_tokens")
	}
	if tt <= 0 {
		tt = addUsageTokens(it, ot)
	}
	return deductCacheRead(it, cr), ot, tt, cr, cc
}

// anthropicUsageFromResponse 非流式 Anthropic 响应用量：SDK v1.56.0 Usage
// 结构体直读（CacheRead/CacheCreationInputTokens 字段存在）。
func anthropicUsageFromResponse(u anthropic.Usage) (it, ot, tt, cr, cc int64) {
	raw := u.RawJSON()
	it, ot, tt, cr, cc = u.InputTokens, u.OutputTokens, 0,
		u.CacheReadInputTokens, u.CacheCreationInputTokens
	// A few Messages-compatible relays return OpenAI usage names even though
	// the response envelope is Anthropic. Preserve billing for those responses
	// using the raw usage object retained by the SDK.
	if it <= 0 {
		it = rawUsageNumber(raw, "input_tokens", "prompt_tokens")
	}
	if ot <= 0 {
		ot = rawUsageNumber(raw, "output_tokens", "completion_tokens")
	}
	if cr <= 0 {
		cr = rawUsageNumber(raw, "cache_read_input_tokens", "cache_read_tokens", "input_tokens_details.cached_tokens", "cached_tokens")
	}
	if cc <= 0 {
		cc = rawUsageNumber(raw, "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation.input_tokens")
	}
	if tt <= 0 {
		tt = rawUsageNumber(raw, "total_tokens")
	}
	if tt <= 0 {
		tt = addUsageTokens(it, ot)
	}
	return it, ot, tt, cr, cc
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
// 计数值 = 功能调用计数（统一计费模型 spec 2026-08-13：落 CallCount——
// 图片生成每张一次功能调用；search 端点接入后另计）。两种输入形态（输出定位
// 路径不同）各一个入口，共享计数核心 countImageItems——调用方按形态直选
// （不做"先试 A 再回退 B"的双扫描）：
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
// 热路径零分配（评审 P2-1 修复，AllocsPerRun==0 测试钉住）：复用 strip_scan
// 同族手写字节扫描（scanKeyValue 定位 + scanTools 元素迭代 + extractKeys 键
// 提取）——gjson GetBytes 对数组值会物化 Raw 字符串（实测 1 alloc/帧；ForEach
// 闭包实测 0 alloc，评审归因修正），字节扫描彻底消除。id 去重走栈数组
// [16][]byte（子切片零拷贝；官方 n 上限 10 零分配兜底；>16 唯一 id 的病态帧
// 去重退化为全数计数——只可能虚增不虚减，防御性接受）。无 image item 的帧
// 短路零分配。
func respImageCountCompleted(data []byte) int64 {
	// completed 帧形态：response.output 信封（先定位 "response" 对象，再定位
	// 其内 "output"——嵌套同名键不误定位）
	respStart, respEnd, ok := scanKeyValue(data, responseKeyBytes)
	if !ok {
		return 0
	}
	sub := data[respStart:respEnd]
	outStart, outEnd, ok := scanKeyValue(sub, outputKeyBytes)
	if !ok {
		return 0
	}
	return countImageItems(sub[outStart:outEnd])
}

// respImageCountBody 非流式响应体形态（SDK RawJSON：无 response 信封，
// output 顶层直定位）。
func respImageCountBody(data []byte) int64 {
	outStart, outEnd, ok := scanKeyValue(data, outputKeyBytes)
	if !ok {
		return 0
	}
	return countImageItems(data[outStart:outEnd])
}

// countImageItems 计数核心（raw = output 数组值字节；调用方完成定位——两
// 形态仅定位路径不同，拆双入口省去 fallback 双扫描）。
func countImageItems(raw []byte) int64 {
	if len(raw) == 0 || raw[0] != '[' {
		return 0 // 缺失/非数组：无 item 可数
	}
	var (
		count int64
		ids   [16][]byte
		n     int
	)
	for tv := range scanTools(raw) {
		if !bytes.Equal(tv.Type, imageGenCallBytes) {
			continue // type 过滤：其他工具 item 的 result 不参与
		}
		if !imageResultNonEmpty(tv.Result) {
			continue // result 缺失/null/空串/空数组：图未生成
		}
		if len(tv.ID) > 0 {
			dup := false
			for j := 0; j < n; j++ {
				if bytes.Equal(ids[j], tv.ID) {
					dup = true
					break
				}
			}
			if dup {
				continue // 已计数（id 去重）
			}
			if n < len(ids) {
				ids[n] = tv.ID
				n++
			}
		} // id 缺失：按出现顺序全数计入（不走去重）
		count++
	}
	return count
}

// imageResultNonEmpty result 非空判定（spec §6：result 必填非空 = 图已生成）：
// 缺失/空串/null 字面量/空数组 → 空；非空字符串（base64）与非空数组
// （image_url 对象形态）→ 非空。与 gjson 版（Exists && Type!=Null && 非空）
// 语义等价；status 不参与（V1-V3 实证终态 status="generating"）。
func imageResultNonEmpty(r []byte) bool {
	if len(r) == 0 {
		return false
	}
	if r[0] == '[' {
		return !(len(r) == 2 && r[1] == ']') // 空数组 → 空
	}
	if r[0] == 'n' && bytes.Equal(r, nullBytes) {
		return false // null 字面量 → 空
	}
	return true
}
