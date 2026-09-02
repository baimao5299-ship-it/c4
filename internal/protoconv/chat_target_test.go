// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func decodeTargetObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func targetArray(t *testing.T, obj map[string]any, key string) []any {
	t.Helper()
	value, ok := obj[key].([]any)
	require.True(t, ok, "%s must be an array", key)
	return value
}

func TestChatTargetDirectionsAreRuntimeOnly(t *testing.T) {
	require.False(t, AutoResponsesToChat.Valid())
	require.False(t, AutoMessagesToChat.Valid())
	require.NotEqual(t, domain.ProtocolConvertAuto, AutoResponsesToChat)
	require.NotEqual(t, domain.ProtocolConvertAuto, AutoMessagesToChat)
}

func TestConvertRequestMessagesToChat(t *testing.T) {
	body := []byte(`{
		"model":"claude-test","system":[{"type":"text","text":"rule A"},{"type":"text","text":"rule B"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"weather?"}]},
			{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"call_1","name":"weather","input":{"city":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"sunny"}]}
		],
		"max_tokens":321,"stop_sequences":["END"],"temperature":0.2,"top_p":0.8,"stream":true,
		"tools":[{"name":"weather","description":"lookup","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"weather"}
	}`)

	out, err := ConvertRequest(body, AutoMessagesToChat)
	require.NoError(t, err)
	got := decodeTargetObject(t, out)
	require.Equal(t, "claude-test", got["model"])
	require.Equal(t, float64(321), got["max_tokens"])
	require.Equal(t, []any{"END"}, got["stop"])
	require.Equal(t, true, got["stream"])

	messages := targetArray(t, got, "messages")
	require.Len(t, messages, 4)
	require.Equal(t, "system", messages[0].(map[string]any)["role"])
	require.Equal(t, "rule A\nrule B", messages[0].(map[string]any)["content"])
	require.Equal(t, "user", messages[1].(map[string]any)["role"])
	assistant := messages[2].(map[string]any)
	require.Equal(t, "assistant", assistant["role"])
	toolCall := targetArray(t, assistant, "tool_calls")[0].(map[string]any)
	require.Equal(t, "call_1", toolCall["id"])
	require.Equal(t, "weather", toolCall["function"].(map[string]any)["name"])
	require.JSONEq(t, `{"city":"x"}`, toolCall["function"].(map[string]any)["arguments"].(string))
	require.Equal(t, "tool", messages[3].(map[string]any)["role"])
	require.Equal(t, "call_1", messages[3].(map[string]any)["tool_call_id"])

	tools := targetArray(t, got, "tools")
	require.Equal(t, "weather", tools[0].(map[string]any)["function"].(map[string]any)["name"])
	require.Equal(t, map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "weather"},
	}, got["tool_choice"])
}

func TestConvertRequestMessagesToChatKeepsToolResultBeforeSameTurnText(t *testing.T) {
	out, err := ConvertRequest([]byte(`{
		"model":"m",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"},{"type":"text","text":"continue"}]}
		]
	}`), AutoMessagesToChat)
	require.NoError(t, err)
	messages := targetArray(t, decodeTargetObject(t, out), "messages")
	require.Len(t, messages, 3)
	require.Equal(t, "assistant", messages[0].(map[string]any)["role"])
	require.Equal(t, "tool", messages[1].(map[string]any)["role"])
	require.Equal(t, "call_1", messages[1].(map[string]any)["tool_call_id"])
	require.Equal(t, "user", messages[2].(map[string]any)["role"])
	require.Equal(t, "continue", messages[2].(map[string]any)["content"])
}

func TestConvertRequestResponsesToChat(t *testing.T) {
	body := []byte(`{
		"model":"gpt-test","instructions":"be concise","input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],"max_output_tokens":99,"stream":true,
		"tools":[{"type":"function","name":"lookup","description":"d","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"lookup"}
	}`)

	out, err := ConvertRequest(body, AutoResponsesToChat)
	require.NoError(t, err)
	got := decodeTargetObject(t, out)
	require.Equal(t, "gpt-test", got["model"])
	require.Equal(t, float64(99), got["max_tokens"])
	require.Equal(t, true, got["stream"])
	messages := targetArray(t, got, "messages")
	require.Equal(t, "system", messages[0].(map[string]any)["role"])
	require.Equal(t, "be concise", messages[0].(map[string]any)["content"])
	require.Equal(t, "assistant", messages[2].(map[string]any)["role"])
	require.Equal(t, "call_1", targetArray(t, messages[2].(map[string]any), "tool_calls")[0].(map[string]any)["id"])
	require.Equal(t, "tool", messages[3].(map[string]any)["role"])
}

func TestConvertResponseChatToMessages(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl_1","object":"chat.completion","created":12,"model":"gpt-test",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3}}
	}`)

	out, err := ConvertResponse(body, AutoMessagesToChat)
	require.NoError(t, err)
	got := decodeTargetObject(t, out)
	require.Equal(t, "message", got["type"])
	require.Equal(t, "assistant", got["role"])
	require.Equal(t, "tool_use", got["stop_reason"])
	content := targetArray(t, got, "content")
	require.Equal(t, "hello", content[0].(map[string]any)["text"])
	require.Equal(t, "tool_use", content[1].(map[string]any)["type"])
	require.Equal(t, "call_1", content[1].(map[string]any)["id"])
	require.Equal(t, float64(10), got["usage"].(map[string]any)["input_tokens"])
	require.Equal(t, float64(3), got["usage"].(map[string]any)["cache_read_input_tokens"])
}

func TestConvertResponseChatToResponses(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl_1","object":"chat.completion","created":12,"model":"gpt-test",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3}}
	}`)

	out, err := ConvertResponse(body, AutoResponsesToChat)
	require.NoError(t, err)
	got := decodeTargetObject(t, out)
	require.Equal(t, "response", got["object"])
	require.Equal(t, "completed", got["status"])
	output := targetArray(t, got, "output")
	require.Len(t, output, 2)
	require.Equal(t, "message", output[0].(map[string]any)["type"])
	require.Equal(t, "function_call", output[1].(map[string]any)["type"])
	require.Equal(t, "call_1", output[1].(map[string]any)["call_id"])
	usage := got["usage"].(map[string]any)
	require.Equal(t, float64(10), usage["input_tokens"])
	require.Equal(t, float64(5), usage["output_tokens"])
	require.Equal(t, float64(15), usage["total_tokens"])
}

func mapChatTarget(t *testing.T, dir domain.ProtocolConvert, payloads ...string) string {
	t.Helper()
	mapper := NewStreamMapper(dir)
	var out []byte
	for _, payload := range payloads {
		frame, drop := mapper.Map("", []byte(payload))
		if !drop {
			validateFrames(t, frame)
			out = append(out, frame...)
		}
	}
	return string(out)
}

func TestMapChatStreamToMessages(t *testing.T) {
	out := mapChatTarget(t, AutoMessagesToChat,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":12,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":12,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":12,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":12,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"x\"}"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":12,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3}}}`,
		`[DONE]`,
	)
	require.Contains(t, out, "event: message_start")
	require.Contains(t, out, `"content_block":{"text":"","type":"text"}`)
	require.Contains(t, out, `"delta":{"text":"hello","type":"text_delta"}`)
	require.Contains(t, out, `"content_block":{"id":"call_1","input":{},"name":"lookup","type":"tool_use"}`)
	require.Contains(t, out, `"delta":{"partial_json":"{\"q\":\"x\"}","type":"input_json_delta"}`)
	require.Contains(t, out, `"stop_reason":"tool_use"`)
	require.Contains(t, out, `"output_tokens":5`)
	require.Contains(t, out, "event: message_stop")
	require.NotContains(t, out, "[DONE]")
}

func TestMapChatStreamBuffersSplitToolMetadata(t *testing.T) {
	out := mapChatTarget(t, AutoMessagesToChat,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"look"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"up"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"x\"}"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	require.Equal(t, 1, strings.Count(out, `"type":"tool_use"`))
	require.Contains(t, out, `"id":"call_1"`)
	require.Contains(t, out, `"name":"lookup"`)
	require.Contains(t, out, `"partial_json":"{\"q\":\"x\"}"`)
}

func TestMapChatStreamMergesSplitAndCumulativeToolMetadata(t *testing.T) {
	out := mapChatTarget(t, AutoMessagesToChat,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_","type":"function","function":{"name":"look"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`,
		`[DONE]`,
	)
	require.Equal(t, 1, strings.Count(out, `"type":"tool_use"`))
	require.Contains(t, out, `"id":"call_1"`)
	require.Contains(t, out, `"name":"lookup"`)
	require.NotContains(t, out, `call_call_1`)
	require.NotContains(t, out, `looklookup`)
}

func TestMapChatStreamToResponses(t *testing.T) {
	out := mapChatTarget(t, AutoResponsesToChat,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":12,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":12,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":12,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3}}}`,
		`[DONE]`,
	)
	require.Contains(t, out, "event: response.created")
	require.Contains(t, out, "event: response.output_item.added")
	require.Contains(t, out, `"delta":"hello"`)
	require.Contains(t, out, "event: response.completed")
	require.Contains(t, out, `"input_tokens":10`)
	require.Contains(t, out, `"output_tokens":5`)
	require.Contains(t, out, `"total_tokens":15`)
	require.NotContains(t, out, "[DONE]")
}

func TestMapChatStreamTerminalStateIsIdempotent(t *testing.T) {
	mapper := NewStreamMapper(AutoMessagesToChat)
	frame, drop := mapper.Map("", []byte(`{"error":{"type":"upstream_error","message":"bad"}}`))
	require.False(t, drop)
	require.Contains(t, string(frame), "event: error")

	_, drop = mapper.Map("", []byte(`{"id":"late","model":"m","choices":[{"index":0,"delta":{"content":"must not leak"}}]}`))
	require.True(t, drop)
	_, drop = mapper.Map("", []byte(`[DONE]`))
	require.True(t, drop)
}
