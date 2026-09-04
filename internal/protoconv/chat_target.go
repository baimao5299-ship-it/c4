// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
)

// These directions are private to automatic runtime negotiation. They are
// deliberately absent from domain.ProtocolConvert.Valid, so an administrator
// cannot persist them as group configuration.
const (
	AutoResponsesToChat domain.ProtocolConvert = "auto_responses_to_chat"
	AutoMessagesToChat  domain.ProtocolConvert = "auto_messages_to_chat"
)

// respToChatRequest composes the two loss-bounded request converters already
// used by explicit protocol conversion. Keeping one canonical Responses parser
// avoids a second interpretation of function_call/call_id semantics.
func respToChatRequest(body []byte) ([]byte, error) {
	messagesBody, err := respToMessRequest(body)
	if err != nil {
		return nil, err
	}
	return messToChatRequest(messagesBody)
}

// messToChatRequest converts an Anthropic Messages request to Chat Completions.
// Text, system instructions, tool calls/results and common sampling controls
// are preserved. Anthropic-only controls without a Chat equivalent are omitted.
func messToChatRequest(body []byte) ([]byte, error) {
	req, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, 10)
	pass(out, req, "model", "temperature", "top_p", "stream", "metadata")
	if v, ok := req["max_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	}
	if v, ok := req["stop_sequences"]; ok && v != nil {
		out["stop"] = v
	}

	messages := make([]any, 0, 8)
	if system, ok := anthropicSystem(req); ok {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	if converted, ok := messMessagesToChat(req); ok {
		messages = append(messages, converted...)
	}
	if len(messages) > 0 {
		out["messages"] = messages
	}
	if tools, ok := arr(req, "tools"); ok {
		out["tools"] = messToolsToChat(tools)
	}
	if choice, parallel, ok := messToolChoiceToChat(req); ok {
		out["tool_choice"] = choice
		if parallel != nil {
			out["parallel_tool_calls"] = *parallel
		}
	}
	return json.Marshal(out)
}

func reasoningEffortBudget(effort string) int {
	switch effort {
	case "low":
		return 1024
	case "high":
		return 4096
	default:
		return 2048
	}
}

func messMessagesToChat(req map[string]any) ([]any, bool) {
	messages, ok := arr(req, "messages")
	if !ok {
		return nil, false
	}
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := str(message, "role")
		switch role {
		case "user":
			text, results := messUserContentToChat(message["content"])
			// Chat requires tool results to follow the assistant tool call before
			// any subsequent user text. Anthropic can carry both block types in
			// one user turn, so split that turn in protocol-valid order.
			out = append(out, results...)
			if text != "" {
				out = append(out, map[string]any{"role": "user", "content": text})
			}
		case "assistant":
			text, calls, reasoning := messAssistantContentToChat(message["content"])
			if text == "" && len(calls) == 0 && reasoning == "" {
				continue
			}
			converted := map[string]any{"role": "assistant", "content": text}
			if len(calls) > 0 {
				converted["tool_calls"] = calls
			}
			if reasoning != "" {
				converted["reasoning_content"] = reasoning
			}
			out = append(out, converted)
		}
	}
	return out, true
}

func messUserContentToChat(content any) (string, []any) {
	if text, ok := content.(string); ok {
		return text, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return "", nil
	}
	var text []string
	var results []any
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if value, ok := str(block, "text"); ok {
				text = append(text, value)
			}
		case "tool_result":
			id, _ := str(block, "tool_use_id")
			value, _ := blockText(block["content"])
			results = append(results, map[string]any{
				"role": "tool", "tool_call_id": id, "content": value,
			})
		}
	}
	return joinStrings(text, "\n"), results
}

func messAssistantContentToChat(content any) (string, []any, string) {
	if text, ok := content.(string); ok {
		return text, nil, ""
	}
	blocks, ok := content.([]any)
	if !ok {
		return "", nil, ""
	}
	var text []string
	var calls []any
	var reasoning []string
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if value, ok := str(block, "text"); ok {
				text = append(text, value)
			}
		case "tool_use":
			id, _ := str(block, "id")
			name, _ := str(block, "name")
			calls = append(calls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": marshalAny(block["input"])},
			})
		case "thinking":
			if value, ok := str(block, "thinking"); ok {
				reasoning = append(reasoning, value)
			}
		}
	}
	return joinStrings(text, ""), calls, joinStrings(reasoning, "")
}

func messToolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		function := map[string]any{}
		pass(function, tool, "name", "description")
		if schema, ok := tool["input_schema"]; ok && schema != nil {
			function["parameters"] = schema
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out
}

func messToolChoiceToChat(req map[string]any) (any, *bool, bool) {
	raw, ok := req["tool_choice"]
	if !ok || raw == nil {
		return nil, nil, false
	}
	if value, ok := raw.(string); ok {
		if value == "any" {
			return "required", nil, true
		}
		return value, nil, true
	}
	choice, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, false
	}
	var parallel *bool
	if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok {
		value := !disabled
		parallel = &value
	}
	switch typ, _ := str(choice, "type"); typ {
	case "tool":
		if name, ok := str(choice, "name"); ok {
			return map[string]any{
				"type": "function", "function": map[string]any{"name": name},
			}, parallel, true
		}
	case "any":
		return "required", parallel, true
	case "auto", "none":
		return typ, parallel, true
	}
	return nil, parallel, false
}

// chatToRespResponse reuses the Messages response shape as a normalized
// intermediate, then the existing Messages->Responses converter produces the
// public Responses object and usage fields.
func chatToRespResponse(body []byte) ([]byte, error) {
	messagesBody, err := chatToMessResponse(body)
	if err != nil {
		return nil, err
	}
	return messToRespResponse(messagesBody)
}

func chatToMessResponse(body []byte) ([]byte, error) {
	response, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	id, _ := str(response, "id")
	model, _ := str(response, "model")
	message := map[string]any{}
	finishReason := ""
	if choices, ok := arr(response, "choices"); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			message, _ = choice["message"].(map[string]any)
			finishReason, _ = str(choice, "finish_reason")
		}
	}
	out := map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content":     chatMessageToMessBlocks(message),
		"stop_reason": chatFinishToMess(finishReason), "stop_sequence": nil,
		"usage": chatUsageToMess(response),
	}
	return json.Marshal(out)
}

func chatMessageToMessBlocks(message map[string]any) []any {
	blocks := make([]any, 0, 4)
	if content, ok := message["content"].(string); ok && content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": content})
	} else if parts, ok := arr(message, "content"); ok {
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "text" {
				continue
			}
			if text, ok := str(part, "text"); ok {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
		}
	}
	if reasoning, ok := firstChatReasoning(message); ok && reasoning != "" {
		blocks = append([]any{map[string]any{"type": "thinking", "thinking": reasoning}}, blocks...)
	}
	if calls, ok := arr(message, "tool_calls"); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			function, ok := call["function"].(map[string]any)
			if !ok {
				continue
			}
			id, _ := str(call, "id")
			name, _ := str(function, "name")
			blocks = append(blocks, map[string]any{
				"type": "tool_use", "id": id, "name": name, "input": parseJSON(function["arguments"]),
			})
		}
	}
	return blocks
}

func chatFinishToMess(reason string) string {
	switch reason {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func chatUsageToMess(response map[string]any) map[string]any {
	input, output, cached := int64(0), int64(0), int64(0)
	if usage, ok := response["usage"].(map[string]any); ok {
		input = intOr0(usage, "prompt_tokens")
		if input == 0 {
			input = intOr0(usage, "input_tokens")
		}
		output = intOr0(usage, "completion_tokens")
		if output == 0 {
			output = intOr0(usage, "output_tokens")
		}
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			cached = intOr0(details, "cached_tokens")
		}
	}
	return map[string]any{
		"input_tokens": input, "output_tokens": output,
		"cache_creation_input_tokens": int64(0), "cache_read_input_tokens": cached,
	}
}

type chatStreamEvent struct {
	name string
	data map[string]any
}

func (m *StreamMapper) mapChatToMess(_ string, data []byte) ([]byte, bool) {
	events := m.chatToMessEvents(data)
	if len(events) == 0 {
		return nil, true
	}
	var out []byte
	for _, event := range events {
		out = append(out, EncodeFrame(event.name, event.data)...)
	}
	return out, false
}

func (m *StreamMapper) mapChatToResp(_ string, data []byte) ([]byte, bool) {
	events := m.chatToMessEvents(data)
	if len(events) == 0 {
		return nil, true
	}
	if m.inner == nil {
		m.inner = NewStreamMapper(domain.ProtocolConvertRespToMess)
	}
	// Chat usage normally arrives in the final chunk, after message_start has
	// already been synthesized. Update the composed mapper before message_stop
	// so response.completed carries the real counters.
	m.inner.it = m.it
	m.inner.ot = m.ot
	m.inner.cached = m.cached
	var out []byte
	for _, event := range events {
		payload, err := json.Marshal(event.data)
		if err != nil {
			continue
		}
		frame, drop := m.inner.Map(event.name, payload)
		if !drop {
			out = append(out, frame...)
		}
	}
	return out, len(out) == 0
}

func (m *StreamMapper) chatToMessEvents(data []byte) []chatStreamEvent {
	if m.done {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return m.finishChatStream()
	}
	event, err := decodeObj(data)
	if err != nil {
		return nil
	}
	if upstreamError, ok := event["error"].(map[string]any); ok {
		if m.done {
			return nil
		}
		m.done = true
		message, _ := str(upstreamError, "message")
		typ, _ := str(upstreamError, "type")
		if typ == "" {
			typ = "api_error"
		}
		return []chatStreamEvent{{name: "error", data: map[string]any{
			"type": "error", "error": map[string]any{"type": typ, "message": message},
		}}}
	}
	if id, ok := str(event, "id"); ok && id != "" {
		m.id = id
	}
	if model, ok := str(event, "model"); ok && model != "" {
		m.model = model
	}
	m.captureChatUsage(event)

	var out []chatStreamEvent
	if !m.started {
		m.started = true
		out = append(out, chatStreamEvent{name: "message_start", data: map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": m.id, "type": "message", "role": "assistant", "model": m.model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens": m.it, "output_tokens": int64(0),
					"cache_creation_input_tokens": int64(0), "cache_read_input_tokens": m.cached,
				},
			},
		}})
	}
	choices, ok := arr(event, "choices")
	if !ok {
		return out
	}
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if index, exists := num(choice, "index"); exists && index != 0 {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			out = append(out, m.chatDeltaToMessEvents(delta)...)
		}
		if reason, ok := str(choice, "finish_reason"); ok && reason != "" {
			m.reason = chatFinishToMess(reason)
		}
		break
	}
	return out
}

func (m *StreamMapper) captureChatUsage(event map[string]any) {
	usage, ok := event["usage"].(map[string]any)
	if !ok {
		return
	}
	m.it = intOr0(usage, "prompt_tokens")
	if m.it == 0 {
		m.it = intOr0(usage, "input_tokens")
	}
	m.ot = intOr0(usage, "completion_tokens")
	if m.ot == 0 {
		m.ot = intOr0(usage, "output_tokens")
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		m.cached = intOr0(details, "cached_tokens")
	}
}

func (m *StreamMapper) chatDeltaToMessEvents(delta map[string]any) []chatStreamEvent {
	m.ensureBlocks()
	var out []chatStreamEvent
	if reasoning, ok := firstChatReasoning(delta); ok && reasoning != "" {
		index := m.chatReasoningBlock()
		if !m.blockStarted[index] {
			m.blockStarted[index] = true
			out = append(out, chatStreamEvent{name: "content_block_start", data: map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}})
		}
		out = append(out, chatStreamEvent{name: "content_block_delta", data: map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
		}})
	}
	if text, ok := str(delta, "content"); ok && text != "" {
		index := m.chatTextBlock()
		if !m.blockStarted[index] {
			m.blockStarted[index] = true
			out = append(out, chatStreamEvent{name: "content_block_start", data: map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "text", "text": ""},
			}})
		}
		out = append(out, chatStreamEvent{name: "content_block_delta", data: map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}})
	}
	toolCalls, ok := arr(delta, "tool_calls")
	if !ok {
		return out
	}
	for _, raw := range toolCalls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		chatIndex := intOr0(call, "index")
		index := m.chatToolBlock(chatIndex)
		if id, ok := str(call, "id"); ok && id != "" {
			m.fcIDs[index] = mergeChatFragment(m.fcIDs[index], id)
		}
		arguments := ""
		if function, ok := call["function"].(map[string]any); ok {
			if name, ok := str(function, "name"); ok && name != "" {
				m.fcNames[index] = mergeChatFragment(m.fcNames[index], name)
			}
			arguments, _ = str(function, "arguments")
		}
		if !m.blockStarted[index] {
			m.argsByIndex[index] += arguments
			// Compatible Chat relays may split id/name across chunks. Delay the
			// immutable Anthropic block header until both metadata fields and the
			// first argument fragment are complete. A no-argument tool is flushed
			// at EOF, which leaves later metadata fragments mergeable.
			if m.fcIDs[index] != "" && m.fcNames[index] != "" && arguments != "" {
				out = append(out, m.startChatToolBlock(index)...)
			}
			continue
		}
		if arguments != "" {
			out = append(out, chatStreamEvent{name: "content_block_delta", data: map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
			}})
		}
	}
	return out
}

func firstChatReasoning(delta map[string]any) (string, bool) {
	for _, key := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
		if value, ok := str(delta, key); ok && value != "" {
			return value, true
		}
	}
	return "", false
}

// mergeChatFragment accepts both delta-style fragments and relays that repeat
// the cumulative value on every chunk. IDs and function names are normally
// sent once, but a few compatibility relays split them; blindly concatenating
// repeated cumulative values produces invalid tool-call identifiers/names.
func mergeChatFragment(current, fragment string) string {
	if fragment == "" {
		return current
	}
	if current == "" || current == fragment {
		return firstNonEmptyChatFragment(current, fragment)
	}
	if strings.HasPrefix(fragment, current) {
		return fragment
	}
	if strings.HasPrefix(current, fragment) {
		return current
	}
	return current + fragment
}

func firstNonEmptyChatFragment(current, fragment string) string {
	if current != "" {
		return current
	}
	return fragment
}

func (m *StreamMapper) chatTextBlock() int64 {
	if !m.chatTextSet {
		m.chatTextSet = true
		m.chatTextIndex = m.nextChatBlock
		m.nextChatBlock++
		m.blockOrder = append(m.blockOrder, m.chatTextIndex)
	}
	return m.chatTextIndex
}

func (m *StreamMapper) chatReasoningBlock() int64 {
	if m.chatReasoningSet {
		return m.chatReasoningIndex
	}
	m.chatReasoningSet = true
	m.chatReasoningIndex = m.nextChatBlock
	m.nextChatBlock++
	m.blockOrder = append(m.blockOrder, m.chatReasoningIndex)
	return m.chatReasoningIndex
}

func (m *StreamMapper) chatToolBlock(chatIndex int64) int64 {
	if index, ok := m.chatToolIndexes[chatIndex]; ok {
		return index
	}
	index := m.nextChatBlock
	m.nextChatBlock++
	m.chatToolIndexes[chatIndex] = index
	m.blockOrder = append(m.blockOrder, index)
	return index
}

func (m *StreamMapper) startChatToolBlock(index int64) []chatStreamEvent {
	if m.blockStarted[index] {
		return nil
	}
	m.blockStarted[index] = true
	id := m.fcIDs[index]
	if id == "" {
		id = "call_" + m.id + "_" + stringIndex(index)
		m.fcIDs[index] = id
	}
	out := []chatStreamEvent{{name: "content_block_start", data: map[string]any{
		"type": "content_block_start", "index": index,
		"content_block": map[string]any{
			"type": "tool_use", "id": id, "name": m.fcNames[index], "input": map[string]any{},
		},
	}}}
	if arguments := m.argsByIndex[index]; arguments != "" {
		out = append(out, chatStreamEvent{name: "content_block_delta", data: map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
		}})
		m.argsByIndex[index] = ""
	}
	return out
}

func (m *StreamMapper) finishChatStream() []chatStreamEvent {
	if m.done {
		return nil
	}
	m.done = true
	var out []chatStreamEvent
	if !m.started {
		m.started = true
		out = append(out, chatStreamEvent{name: "message_start", data: map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": m.id, "type": "message", "role": "assistant", "model": m.model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": m.it, "output_tokens": int64(0)},
			},
		}})
	}
	for _, index := range m.blockOrder {
		if !m.blockStarted[index] && (!m.chatTextSet || index != m.chatTextIndex) {
			out = append(out, m.startChatToolBlock(index)...)
		}
	}
	for _, index := range m.blockOrder {
		if m.blockStarted[index] && !m.blockStopped[index] {
			m.blockStopped[index] = true
			out = append(out, chatStreamEvent{name: "content_block_stop", data: map[string]any{
				"type": "content_block_stop", "index": index,
			}})
		}
	}
	reason := m.reason
	if reason == "" {
		reason = "end_turn"
	}
	out = append(out,
		chatStreamEvent{name: "message_delta", data: map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": m.ot},
		}},
		chatStreamEvent{name: "message_stop", data: map[string]any{"type": "message_stop"}},
	)
	return out
}

func stringIndex(index int64) string {
	return strconv.FormatInt(index, 10)
}
