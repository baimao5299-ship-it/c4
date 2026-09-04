// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"encoding/json"
)

// respToMessRequest 客户端 resp 请求体 → anthropic messages 请求体。字段映射
// 按 Messages API 规范：
//   - instructions + input 内 system/developer 消息项 → 顶层 system（拼接）
//   - input → messages：message 项 → user/assistant 消息（文本块）；
//     function_call 项 → 追加到最近 assistant 消息的 tool_use 块；
//     function_call_output 项 → user 消息的 tool_result 块
//   - max_output_tokens → max_tokens（anthropic 必填，客户端缺失则不补——
//     补差值属策略决定，不由转换器发明）
//   - tools → tools（parameters → input_schema）；tool_choice 归一化
//     （required → any；{type:"function",name} → {type:"tool",name}）
//   - 同名字段透传：model/temperature/top_p/stream/metadata
//   - anthropic 无对应参数（top_logprobs/seed/store/parallel_tool_calls/
//     text/include/truncation 等）→ 按规范丢弃；thinking 映射为 Responses
//     reasoning 的最近 effort 等级
func respToMessRequest(body []byte) ([]byte, error) {
	req, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, 8)
	if sys, ok := respInstructions(req); ok {
		out["system"] = sys
	}
	if msgs, ok := respInputToMessMessages(req); ok {
		out["messages"] = msgs
	} else if s, ok := str(req, "input"); ok {
		// input 为纯字符串形态（Responses API 允许）→ 单条 user 消息
		out["messages"] = []any{map[string]any{"role": "user", "content": s}}
	}
	pass(out, req, "model", "temperature", "top_p", "stream", "metadata")
	if v, ok := req["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if tools, ok := arr(req, "tools"); ok {
		out["tools"] = respToolsToMess(tools)
	}
	if tc, ok := respToolChoice(req); ok {
		out["tool_choice"] = tc
	}
	if reasoning, ok := req["reasoning"]; ok {
		if thinking, ok := reasoningToThinking(reasoning); ok {
			out["thinking"] = thinking
		}
	}
	return json.Marshal(out)
}

// respInstructions instructions + input 内 system/developer 消息项 → 拼接系统文本。
func respInstructions(req map[string]any) (string, bool) {
	var parts []string
	if ins, ok := str(req, "instructions"); ok && ins != "" {
		parts = append(parts, ins)
	}
	if input, ok := arr(req, "input"); ok {
		for _, item := range input {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := str(im, "role")
			if role != "system" && role != "developer" {
				continue
			}
			if t, ok := inputItemText(im); ok {
				parts = append(parts, t)
			}
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return joinStrings(parts, "\n"), true
}

// inputItemText resp input message 项（system/developer）→ 拼接文本。
func inputItemText(im map[string]any) (string, bool) {
	var parts []string
	// Responses accepts the compact message form `content: "text"` in
	// addition to the typed content-part array. Preserve it when folding
	// system/developer items into Anthropic's top-level system field.
	if content, ok := im["content"].(string); ok {
		return content, true
	}
	if content, ok := arr(im, "content"); ok {
		for _, part := range content {
			if pm, ok := part.(map[string]any); ok {
				if t, ok := str(pm, "text"); ok {
					parts = append(parts, t)
				}
			}
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return joinStrings(parts, "\n"), true
}

// respInputToMessMessages resp input → anthropic messages：message 项 → 消息
// （文本块）；function_call 项 → 最近 assistant 消息追加 tool_use 块；
// function_call_output 项 → user 消息 tool_result 块。input_image 等图像
// 透传属 W4 范围，按规范丢弃。
func respInputToMessMessages(req map[string]any) ([]any, bool) {
	input, ok := arr(req, "input")
	if !ok {
		return nil, false
	}
	var msgs []any
	lastAssistant := -1
	for _, item := range input {
		im, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch im["type"] {
		case "message":
			role, _ := str(im, "role")
			if role != "user" && role != "assistant" {
				continue // system/developer 已并入 system
			}
			var blocks []any
			if content, ok := im["content"].(string); ok {
				blocks = append(blocks, map[string]any{"type": "text", "text": content})
			} else if content, ok := arr(im, "content"); ok {
				for _, part := range content {
					pm, ok := part.(map[string]any)
					if !ok {
						continue
					}
					switch pm["type"] {
					case "input_text", "output_text":
						if t, ok := str(pm, "text"); ok {
							blocks = append(blocks, map[string]any{"type": "text", "text": t})
						}
					case "reasoning":
						if t, ok := str(pm, "text"); ok {
							blocks = append(blocks, map[string]any{"type": "thinking", "thinking": t})
						}
					}
				}
			}
			if len(blocks) == 0 {
				continue
			}
			msg := map[string]any{"role": role, "content": blocks}
			msgs = append(msgs, msg)
			if role == "assistant" {
				lastAssistant = len(msgs) - 1
			} else {
				// A later function_call belongs to a new assistant turn, not
				// to an assistant message from before this user turn.
				lastAssistant = -1
			}
		case "function_call":
			if lastAssistant < 0 {
				// Responses returns function_call items as standalone input
				// items. When a caller replays response.output there is no
				// enclosing assistant message to attach to, so synthesize one
				// instead of silently dropping the tool call.
				msgs = append(msgs, map[string]any{"role": "assistant", "content": []any{}})
				lastAssistant = len(msgs) - 1
			}
			am, _ := msgs[lastAssistant].(map[string]any)
			content, _ := arr(am, "content")
			// call_id 优先（M-1 同缺陷：后续 function_call_output.call_id →
			// tool_result.tool_use_id 必须命中 tool_use.id，否则多轮链断裂）
			id := toolCallID(im)
			name, _ := str(im, "name")
			content = append(content, map[string]any{
				"type": "tool_use", "id": id, "name": name, "input": parseJSON(im["arguments"]),
			})
			am["content"] = content
		case "reasoning":
			if lastAssistant < 0 {
				msgs = append(msgs, map[string]any{"role": "assistant", "content": []any{}})
				lastAssistant = len(msgs) - 1
			}
			am, _ := msgs[lastAssistant].(map[string]any)
			content, _ := arr(am, "content")
			if summary, ok := arr(im, "summary"); ok {
				for _, raw := range summary {
					if sm, ok := raw.(map[string]any); ok {
						if t, ok := str(sm, "text"); ok {
							content = append(content, map[string]any{"type": "thinking", "thinking": t})
						}
					}
				}
			}
			am["content"] = content
		case "function_call_output":
			callID, _ := str(im, "call_id")
			output, _ := blockText(im["output"])
			msgs = append(msgs, map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": output}},
			})
			lastAssistant = -1
		}
	}
	return msgs, true
}

// respToolsToMess resp tools → anthropic tools：{type:"function", name,
// description, parameters} → {name, description, input_schema}；非 function
// 内置工具（web_search 等，anthropic 无对应）按规范丢弃。
func respToolsToMess(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok || tm["type"] != "function" {
			continue
		}
		tool := map[string]any{}
		if name, ok := str(tm, "name"); ok {
			tool["name"] = name
		}
		if desc, ok := str(tm, "description"); ok {
			tool["description"] = desc
		}
		if p, ok := tm["parameters"].(map[string]any); ok {
			tool["input_schema"] = p
		}
		out = append(out, tool)
	}
	return out
}

// respToolChoice resp tool_choice → anthropic tool_choice："auto"/"none" 透传；
// "required" → "any"；{type:"function", name} → {type:"tool", name}。
func respToolChoice(req map[string]any) (any, bool) {
	v, ok := req["tool_choice"]
	if !ok || v == nil {
		return nil, false
	}
	if s, ok := v.(string); ok {
		if s == "required" {
			return "any", true
		}
		return s, true
	}
	if m, ok := v.(map[string]any); ok && m["type"] == "function" {
		if name, ok := str(m, "name"); ok {
			return map[string]any{"type": "tool", "name": name}, true
		}
	}
	return nil, false
}

// messToRespResponse anthropic message 对象 → resp 响应对象（非流式）：
// content text 块 → message 项（output_text）；tool_use 块 → function_call
// 项（input 对象 → arguments JSON 字符串）；usage → input/output/total +
// cache_read → input_tokens_details.cached_tokens。
func messToRespResponse(body []byte) ([]byte, error) {
	msg, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	id, _ := str(msg, "id")
	model, _ := str(msg, "model")
	output, it, ot := messContentToRespOutput(msg)
	out := map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          0, // anthropic message 无时间戳（转换器纯函数，不发明）
		"status":              "completed",
		"model":               model,
		"output":              output,
		"parallel_tool_calls": true,
		"usage":               messUsageToResp(msg, it, ot),
	}
	return json.Marshal(out)
}

// messContentToRespOutput anthropic content → resp output 项
// （message 项 + function_call 项），返回 (output, inputTokens, outputTokens)。
func messContentToRespOutput(msg map[string]any) ([]any, int64, int64) {
	var output []any
	var textParts []any
	var fcs []any
	msgID, _ := str(msg, "id")
	content, _ := arr(msg, "content")
	for _, blk := range content {
		bm, ok := blk.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "thinking":
			if t, ok := str(bm, "thinking"); ok {
				output = append(output, map[string]any{
					"id": "rs_" + msgID + "_" + stringIndex(int64(len(output))), "type": "reasoning", "status": "completed",
					"summary": []any{map[string]any{"type": "summary_text", "text": t}},
				})
			}
		case "text":
			if t, ok := str(bm, "text"); ok {
				textParts = append(textParts, map[string]any{"type": "output_text", "text": t, "annotations": []any{}})
			}
		case "tool_use":
			id, _ := str(bm, "id")
			name, _ := str(bm, "name")
			fcs = append(fcs, map[string]any{
				"type": "function_call", "id": id, "call_id": id,
				"name": name, "arguments": marshalAny(bm["input"]), "status": "completed",
			})
		}
	}
	if len(textParts) > 0 {
		id, _ := str(msg, "id")
		output = append(output, map[string]any{
			"id": id, "type": "message", "status": "completed", "role": "assistant",
			"content": textParts,
		})
	}
	output = append(output, fcs...)
	if len(output) == 0 {
		output = []any{}
	}
	it, ot := int64(0), int64(0)
	if u, ok := msg["usage"].(map[string]any); ok {
		it = intOr0(u, "input_tokens")
		ot = intOr0(u, "output_tokens")
	}
	return output, it, ot
}

// messUsageToResp anthropic usage → resp usage。
func messUsageToResp(msg map[string]any, it, ot int64) map[string]any {
	cached := int64(0)
	if u, ok := msg["usage"].(map[string]any); ok {
		cached = intOr0(u, "cache_read_input_tokens")
	}
	return map[string]any{
		"input_tokens":         it,
		"output_tokens":        ot,
		"total_tokens":         sumTokens(it, ot),
		"input_tokens_details": map[string]any{"cached_tokens": cached},
	}
}

// mapMessToResp 流式：anthropic messages SSE 事件 → resp 流。事件映射表：
//
//	message_start           → response.created（id/model/input 用量入状态）
//	content_block_start(thinking) → response.output_item.added(reasoning)
//	content_block_start     → response.output_item.added（text → message 项 /
//	                          tool_use → function_call 项）
//	content_block_delta     → response.output_text.delta（text）/
//	                          response.function_call_arguments.delta（json）
//	message_delta           → 状态吸收（output_tokens/stop_reason），不产帧
//	message_stop            → response.completed（输出项累积 + 用量）
//	error                   → resp 错误帧形态
//	其余 → 丢弃
func (m *StreamMapper) mapMessToResp(name string, data []byte) ([]byte, bool) {
	m.ensureBlocks() // 块级累积 map 懒初始化（评审 I-4）
	ev, err := decodeObj(data)
	if err != nil {
		return nil, true
	}
	switch name {
	case "message_start":
		if m.started {
			return nil, true
		}
		m.started = true
		if msg, ok := ev["message"].(map[string]any); ok {
			m.id, _ = str(msg, "id")
			m.model, _ = str(msg, "model")
			if u, ok := msg["usage"].(map[string]any); ok {
				m.it = intOr0(u, "input_tokens")
				m.cached = intOr0(u, "cache_read_input_tokens")
			}
		}
		return EncodeFrame("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": m.id, "object": "response", "created_at": 0, "status": "in_progress",
				"model": m.model, "output": []any{}, "parallel_tool_calls": true, "usage": nil,
			},
		}), false
	case "content_block_start":
		index := intOr0(ev, "index")
		block, ok := ev["content_block"].(map[string]any)
		if !ok {
			return nil, true
		}
		switch block["type"] {
		case "thinking":
			if m.blockStarted[index] {
				return nil, true
			}
			m.blockStarted[index] = true
			m.blockOrder = append(m.blockOrder, index)
			thinking, _ := str(block, "thinking")
			m.reasoningByIndex[index] = thinking
			f := EncodeFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": index,
				"item": map[string]any{
					"id": m.itemID(index), "type": "reasoning", "status": "in_progress",
					"summary": []any{map[string]any{"type": "summary_text", "text": ""}},
				},
			})
			if thinking != "" {
				f = append(f, EncodeFrame("response.reasoning_summary_text.delta", map[string]any{
					"type": "response.reasoning_summary_text.delta", "item_id": m.itemID(index),
					"output_index": index, "summary_index": 0, "delta": thinking,
				})...)
			}
			return f, false
		case "text":
			if m.blockStarted[index] {
				return nil, true
			}
			m.blockStarted[index] = true
			m.blockOrder = append(m.blockOrder, index)
			item := map[string]any{
				"id": m.itemID(index), "type": "message", "status": "in_progress",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
			}
			return EncodeFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": index, "item": item,
			}), false
		case "tool_use":
			if m.blockStarted[index] {
				return nil, true
			}
			m.blockStarted[index] = true
			m.blockOrder = append(m.blockOrder, index)
			id, _ := str(block, "id")
			name, _ := str(block, "name")
			m.fcIDs[index] = id
			m.fcNames[index] = name
			item := map[string]any{
				"id": "fc_" + id, "type": "function_call", "call_id": id,
				"name": name, "arguments": "", "status": "in_progress",
			}
			return EncodeFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": index, "item": item,
			}), false
		}
		return nil, true
	case "content_block_delta":
		index := intOr0(ev, "index")
		delta, ok := ev["delta"].(map[string]any)
		if !ok {
			return nil, true
		}
		switch delta["type"] {
		case "thinking_delta":
			thinking, _ := str(delta, "thinking")
			m.reasoningByIndex[index] += thinking
			return EncodeFrame("response.reasoning_summary_text.delta", map[string]any{
				"type": "response.reasoning_summary_text.delta", "item_id": m.itemID(index),
				"output_index": index, "summary_index": 0, "delta": thinking,
			}), false
		case "text_delta":
			text, _ := str(delta, "text")
			m.textByIndex[index] += text
			return EncodeFrame("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": m.itemID(index),
				"output_index": index, "content_index": 0, "delta": text,
			}), false
		case "input_json_delta":
			partial, _ := str(delta, "partial_json")
			m.argsByIndex[index] += partial
			return EncodeFrame("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": "fc_" + m.fcIDs[index],
				"output_index": index, "delta": partial,
			}), false
		}
		return nil, true
	case "content_block_stop":
		index := intOr0(ev, "index")
		if _, ok := m.reasoningByIndex[index]; ok && m.blockStarted[index] && !m.blockStopped[index] {
			m.blockStopped[index] = true
			return EncodeFrame("response.reasoning_summary_text.done", map[string]any{
				"type": "response.reasoning_summary_text.done", "item_id": m.itemID(index),
				"output_index": index, "summary_index": 0, "text": m.reasoningByIndex[index],
			}), false
		}
		return nil, true
	case "message_delta":
		if u, ok := ev["usage"].(map[string]any); ok {
			m.ot = intOr0(u, "output_tokens")
		}
		if d, ok := ev["delta"].(map[string]any); ok {
			m.reason, _ = str(d, "stop_reason")
		}
		return nil, true
	case "message_stop":
		if m.done {
			return nil, true
		}
		m.done = true
		return EncodeFrame("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": m.id, "object": "response", "created_at": 0, "status": "completed",
				"model": m.model, "output": m.messOutputItems(), "parallel_tool_calls": true,
				"usage": map[string]any{
					"input_tokens": m.it, "output_tokens": m.ot, "total_tokens": sumTokens(m.it, m.ot),
					"input_tokens_details": map[string]any{"cached_tokens": m.cached},
				},
			},
		}), false
	case "error":
		if e, ok := ev["error"].(map[string]any); ok {
			msg, _ := str(e, "message")
			typ, _ := str(e, "type")
			return EncodeFrame("error", map[string]any{"code": typ, "message": msg}), false
		}
		return nil, true
	}
	return nil, true
}

// messOutputItems 累积的块级输出 → response.completed 的 output 数组
// （message 项补全文本、function_call 项补全 arguments + completed 状态）。
func (m *StreamMapper) messOutputItems() []any {
	var output []any
	for _, index := range m.blockOrder {
		if text, ok := m.reasoningByIndex[index]; ok {
			output = append(output, map[string]any{
				"id": m.itemID(index), "type": "reasoning", "status": "completed",
				"summary": []any{map[string]any{"type": "summary_text", "text": text}},
			})
			continue
		}
		if name, ok := m.fcNames[index]; ok {
			output = append(output, map[string]any{
				"id": "fc_" + m.fcIDs[index], "type": "function_call", "call_id": m.fcIDs[index],
				"name": name, "arguments": m.argsByIndex[index], "status": "completed",
			})
			continue
		}
		output = append(output, map[string]any{
			"id": m.itemID(index), "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": m.textByIndex[index], "annotations": []any{}}},
		})
	}
	if output == nil {
		return []any{}
	}
	return output
}
