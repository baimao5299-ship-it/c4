// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// 提取单测用真实上游 JSON 构造（评审 I-1：不得用结构体 marshal 自证——
// RawJSON 路径必须经过 SDK UnmarshalJSON 才能得到原始字节）。

// —— chat 流式 usage 帧（顶层 usage.*；cached_tokens 嵌套于
// prompt_tokens_details，与 SDK CompletionUsage 结构体一致——评审 I-1） ——

func TestChatStreamUsage(t *testing.T) {
	frame := []byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"prompt_tokens_details":{"cached_tokens":5},"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":2}}}`)
	pt, ct, tt, cr, cc := chatStreamUsage(frame)
	require.Equal(t, int64(10), pt)
	require.Equal(t, int64(20), ct)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(5), cr, "prompt_tokens_details.cached_tokens 直读")
	require.Equal(t, int64(6), cc, "ephemeral 5m+1h 聚合")

	// 缺失/显式 null → 0（不阻塞采集）
	_, _, _, cr, cc = chatStreamUsage([]byte(`{"usage":{}}`))
	require.Zero(t, cr)
	require.Zero(t, cc)
	_, _, _, cr, cc = chatStreamUsage([]byte(`{"usage":{"prompt_tokens_details":{"cached_tokens":null}}}`))
	require.Zero(t, cr, "显式 null 与缺失等价")
	require.Zero(t, cc)
}

// —— chat 非流式（SDK UnmarshalJSON → PromptTokensDetails 直读 + RawJSON gjson） ——

func TestChatUsageFromResponse(t *testing.T) {
	raw := `{"id":"x","object":"chat.completion","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"prompt_tokens_details":{"cached_tokens":5},"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":2}}}`
	var resp openai.ChatCompletion
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	require.True(t, resp.JSON.Usage.Valid())
	pt, ct, tt, cr, cc := chatUsageFromResponse(resp.Usage)
	require.Equal(t, int64(10), pt)
	require.Equal(t, int64(20), ct)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(5), cr, "SDK PromptTokensDetails.CachedTokens 直读")
	require.Equal(t, int64(6), cc, "RawJSON 保留上游原始字节 → ephemeral 聚合")

	// 无 cache 字段 → 0
	plain := `{"id":"x","object":"chat.completion","model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	var p openai.ChatCompletion
	require.NoError(t, json.Unmarshal([]byte(plain), &p))
	_, _, _, cr, cc = chatUsageFromResponse(p.Usage)
	require.Zero(t, cr)
	require.Zero(t, cc)
}

// —— Anthropic 流式（message.usage.* 前缀） ——

func TestAnthropicStreamUsage(t *testing.T) {
	start := []byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}}`)
	pt, cr, cc := anthropicStartUsage(start)
	require.Equal(t, int64(10), pt)
	require.Equal(t, int64(7), cr, "message.usage.cache_read_input_tokens")
	require.Equal(t, int64(3), cc, "message.usage.cache_creation_input_tokens")

	delta := []byte(`{"type":"message_delta","usage":{"output_tokens":20}}`)
	require.Equal(t, int64(20), anthropicDeltaOutput(delta))

	// 缺 cache 字段 → 0
	_, cr, cc = anthropicStartUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	require.Zero(t, cr)
	require.Zero(t, cc)
}

// —— Anthropic 非流式（SDK 结构体直读） ——

func TestAnthropicUsageFromResponse(t *testing.T) {
	raw := `{"id":"x","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}`
	var resp anthropic.Message
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	require.True(t, resp.JSON.Usage.Valid())
	pt, ct, tt, cr, cc := anthropicUsageFromResponse(resp.Usage)
	require.Equal(t, int64(10), pt)
	require.Equal(t, int64(20), ct)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(7), cr)
	require.Equal(t, int64(3), cc)
}

// —— Responses 流式（response.usage.* 前缀） ——

func TestResponsesStreamUsage(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"id":"r","model":"m","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":5}},"output":[]}}`)
	pt, ct, tt, cr, cc := responsesCompletedUsage(completed)
	require.Equal(t, int64(10), pt)
	require.Equal(t, int64(20), ct)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(5), cr, "response.usage.input_tokens_details.cached_tokens")
	require.Zero(t, cc, "Responses 无 cache_creation 对象，恒 0 预期（M4）")
}

// —— codex resp 顶层 usage（P1-1——T6：SDK 路径 usage 形状为顶层；fixture 对齐
// codex-sdk responses_test.go respUsage 形状 + cache 明细） ——

func TestResponsesTopLevelUsage(t *testing.T) {
	// 流式 completed 帧形态（顶层 usage——response 对象内无 usage）
	completed := []byte(`{"type":"response.completed","response":{"id":"r","object":"response","status":"completed"},"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2},"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":3}}}`)
	it, ot, tt, cr, cc := responsesTopLevelUsage(completed)
	require.Equal(t, int64(10), it)
	require.Equal(t, int64(20), ot)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(2), cr, "顶层 usage.input_tokens_details.cached_tokens")
	require.Equal(t, int64(4), cc, "顶层 cache_creation ephemeral 5m+1h 聚合")

	// 合成体形态（无 type 字段——usage 同样顶层）
	composite := []byte(`{"id":"resp_001","object":"response","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2}}}`)
	it, ot, tt, cr, cc = responsesTopLevelUsage(composite)
	require.Equal(t, int64(10), it)
	require.Equal(t, int64(20), ot)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(2), cr)
	require.Zero(t, cc, "无 cache_creation → 0")

	// 缺失/显式 null → 0（不阻塞采集）
	_, _, _, cr, cc = responsesTopLevelUsage([]byte(`{"usage":{}}`))
	require.Zero(t, cr)
	require.Zero(t, cc)
	_, _, _, cr, cc = responsesTopLevelUsage([]byte(`{"usage":{"input_tokens_details":{"cached_tokens":null}}}`))
	require.Zero(t, cr, "显式 null 与缺失等价")
}

func TestSniffResponsesCompletedTop(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"id":"r","object":"response","status":"completed"},"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2},"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":3}}}`)
	u, ok := sniffResponsesCompletedTop(completed)
	require.True(t, ok, "completed 帧命中")
	require.Equal(t, int64(10), u.it)
	require.Equal(t, int64(20), u.ot)
	require.Equal(t, int64(30), u.tt)
	require.Equal(t, int64(2), u.cr)
	require.Equal(t, int64(4), u.cc)

	// 精确判定：正文含 "type":"response.completed" 子串的**非 completed 帧**
	//（消息文本）不命中——WS 路径 bytes.Contains 预筛会误命中（P1-1 冻结防线）
	messageFrame := []byte(`{"type":"message","content":[{"type":"output_text","text":"say {\"type\":\"response.completed\"} please"}]}`)
	_, ok = sniffResponsesCompletedTop(messageFrame)
	require.False(t, ok, "正文含子串的非 completed 帧不得命中（type 精确判定）")

	// 非 JSON 行 / 未知类型 → 不命中
	_, ok = sniffResponsesCompletedTop([]byte(`this is not json`))
	require.False(t, ok)
	_, ok = sniffResponsesCompletedTop([]byte(`{"type":"output_item.done","item":{"id":"m"}}`))
	require.False(t, ok)

	// completed 但 usage 缺失 → 命中 + 全 0（缺失 = 0，不阻塞采集）
	u, ok = sniffResponsesCompletedTop([]byte(`{"type":"response.completed","response":{"id":"r"}}`))
	require.True(t, ok)
	require.Zero(t, u.it)
	require.Zero(t, u.cc)
}

// —— Responses 非流式（直读 + RawJSON） ——

func TestResponsesUsageFromResponse(t *testing.T) {
	raw := `{"id":"r","object":"response","model":"m","status":"completed","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":5}},"output":[]}`
	var resp responses.Response
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	require.True(t, resp.JSON.Usage.Valid())
	pt, ct, tt, cr, cc := responsesUsageFromResponse(resp.Usage)
	require.Equal(t, int64(10), pt)
	require.Equal(t, int64(20), ct)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(5), cr, "SDK InputTokensDetails.CachedTokens 直读")
	require.Zero(t, cc, "恒 0 预期（M4）")
}

// —— buildLog 接线（评审 I-2）：cr/cc → UsageLog.CacheRead/CreationTokens ——

func TestBuildLogWiresCacheTokens(t *testing.T) {
	l := (&Proxy{}).buildLog("req1", 1, 2, "m", "m", domain.FormatOpenAIChat, 200, domain.ErrNone,
		usageTuple{it: 10, ot: 20, tt: 30, cr: 4, cc: 6}, time.Now())
	require.Equal(t, int64(4), l.CacheReadTokens)
	require.Equal(t, int64(6), l.CacheCreationTokens)

	nilU := (&Proxy{}).buildLog("req2", 1, 2, "m", "m", domain.FormatOpenAIChat, 200, domain.ErrNone, usageTuple{}, time.Now())
	require.Zero(t, nilU.CacheReadTokens, "零值元组 → 0（不 panic）")
	require.Zero(t, nilU.CacheCreationTokens)
}

// —— mappedFor 判定（评审 I-1）：映射/无映射/used 空 ——

func TestMappedFor(t *testing.T) {
	require.Equal(t, "gpt-4o-upstream", mappedFor("gpt-4o", "gpt-4o-upstream"), "有映射 → 映射后模型")
	require.Equal(t, "", mappedFor("gpt-4o", "gpt-4o"), "无映射（used == 请求模型）→ 空")
	require.Equal(t, "", mappedFor("gpt-4o", ""), "used 空（Select 失败未使用任何账号）→ 空")
	require.Equal(t, "", mappedFor("", ""), "请求模型缺失（401）→ 空")
}

// —— buildLog 模型语义（评审 I-1）：Model=客户端请求模型、MappedModel=映射后模型 ——

func TestBuildLogModelSemantics(t *testing.T) {
	mapped := (&Proxy{}).buildLog("r1", 1, 2, "gpt-4o", "gpt-4o-upstream", domain.FormatOpenAIChat, 200, domain.ErrNone, usageTuple{}, time.Now())
	require.Equal(t, "gpt-4o", mapped.Model, "Model = 客户端请求模型")
	require.Equal(t, "gpt-4o-upstream", mapped.MappedModel, "MappedModel = 映射后实际模型")

	plain := (&Proxy{}).buildLog("r2", 1, 2, "gpt-4o", "gpt-4o", domain.FormatOpenAIChat, 200, domain.ErrNone, usageTuple{}, time.Now())
	require.Equal(t, "gpt-4o", plain.Model)
	require.Equal(t, "", plain.MappedModel, "无映射 → MappedModel 空")
}
