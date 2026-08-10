package protoconv

import (
	"encoding/json"
)

// chatToRespRequest 客户端 chat 请求体 → resp 请求体。字段映射按 Responses
// API 规范：
//   - messages → input：system → developer 消息项（Responses 规范 system 已
//     废弃，developer 等价）；user/assistant → message 项（assistant 文本 →
//     output_text 部件）；assistant tool_calls → 独立 function_call 项；
//     tool 消息 → function_call_output 项
//   - max_tokens / max_completion_tokens → max_output_tokens（两者都给时以
//     max_completion_tokens 为准，与 chat 语义一致）
//   - tools → tools（{type:"function"} 内嵌扁平化，strict 透传）
//   - tool_choice → tool_choice（{type:"function",function:{name}} 扁平化）
//   - 同名字段透传：model/temperature/top_p/stream/parallel_tool_calls/
//     user/metadata/store
//   - resp 无对应参数（stop/stream_options/frequency_penalty/presence_penalty/
//     logprobs/seed/n/response_format/audio/logit_bias）→ 按规范丢弃
func chatToRespRequest(body []byte) ([]byte, error) {
	req, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, 8)
	if items, ok := chatMessagesToInput(req); ok {
		out["input"] = items
	}
	pass(out, req, "model", "temperature", "top_p", "stream", "parallel_tool_calls", "user", "metadata", "store")
	if v, ok := req["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	} else if v, ok := req["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if tools, ok := arr(req, "tools"); ok {
		out["tools"] = chatToolsToResp(tools)
	}
	if tc, ok := chatToolChoice(req); ok {
		out["tool_choice"] = tc
	}
	return json.Marshal(out)
}

// chatMessagesToInput chat messages 数组 → resp input items。
func chatMessagesToInput(req map[string]any) ([]any, bool) {
	msgs, ok := arr(req, "messages")
	if !ok {
		return nil, false
	}
	items := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := str(mm, "role")
		switch role {
		case "system":
			if t, ok := contentText(mm["content"]); ok && t != "" {
				items = append(items, map[string]any{
					"type":    "message",
					"role":    "developer",
					"content": []any{map[string]any{"type": "input_text", "text": t}},
				})
			}
		case "user":
			if parts := chatContentToRespParts(mm["content"]); len(parts) > 0 {
				items = append(items, map[string]any{"type": "message", "role": "user", "content": parts})
			}
		case "assistant":
			if parts := chatContentToRespParts(mm["content"]); len(parts) > 0 {
				items = append(items, map[string]any{"type": "message", "role": "assistant", "content": parts})
			}
			// assistant tool_calls → 独立 function_call 项（保持原顺序）
			if tcs, ok := arr(mm, "tool_calls"); ok {
				for _, tc := range tcs {
					tcm, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					id, _ := str(tcm, "id")
					if fn, ok := tcm["function"].(map[string]any); ok {
						name, _ := str(fn, "name")
						args, _ := str(fn, "arguments")
						items = append(items, map[string]any{
							"type": "function_call", "id": id, "call_id": id,
							"name": name, "arguments": args,
						})
					}
				}
			}
		case "tool":
			if out, ok := contentText(mm["content"]); ok {
				callID, _ := str(mm, "tool_call_id")
				items = append(items, map[string]any{"type": "function_call_output", "call_id": callID, "output": out})
			}
		case "function":
			// 已废弃 chat function 消息 → function_call_output（call_id 用 name 近似）
			if out, ok := contentText(mm["content"]); ok {
				name, _ := str(mm, "name")
				items = append(items, map[string]any{"type": "function_call_output", "call_id": name, "output": out})
			}
		}
	}
	return items, true
}

// contentText chat 消息 content（string 或 text 部件数组）→ 拼接文本。
func contentText(content any) (string, bool) {
	switch c := content.(type) {
	case string:
		return c, true
	case []any:
		var parts []string
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
				if t, ok := str(pm, "text"); ok {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return joinStrings(parts, "\n"), true
	}
	return "", false
}

// chatContentToRespParts chat 消息 content → resp 内容部件：text → input_text；
// image_url → input_image（兼容 string 与 {url} 两种形态）；其余部件按规范丢弃。
func chatContentToRespParts(content any) []any {
	switch c := content.(type) {
	case string:
		return []any{map[string]any{"type": "input_text", "text": c}}
	case []any:
		parts := make([]any, 0, len(c))
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text":
				if t, ok := str(pm, "text"); ok {
					parts = append(parts, map[string]any{"type": "input_text", "text": t})
				}
			case "image_url":
				if u, ok := imageURL(pm); ok {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": u})
				}
			}
		}
		return parts
	}
	return nil
}

// imageURL 提取 chat image_url 部件的 URL（string 或 {"url": ...}）。
func imageURL(m map[string]any) (string, bool) {
	if u, ok := str(m, "image_url"); ok {
		return u, true
	}
	if um, ok := m["image_url"].(map[string]any); ok {
		return str(um, "url")
	}
	return "", false
}

// chatToolsToResp chat tools → resp tools：{type:"function", function:{...}}
// 嵌套扁平化为 {type:"function", name, description, parameters, strict}；
// 非 function 工具（web_search_preview 等，resp 结构不同）按规范丢弃。
func chatToolsToResp(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		tool := map[string]any{"type": "function"}
		if name, ok := str(fn, "name"); ok {
			tool["name"] = name
		}
		if desc, ok := str(fn, "description"); ok {
			tool["description"] = desc
		}
		if p, ok := fn["parameters"]; ok && p != nil {
			tool["parameters"] = p
		}
		if s, ok := fn["strict"].(bool); ok {
			tool["strict"] = s
		}
		out = append(out, tool)
	}
	return out
}

// chatToolChoice chat tool_choice → resp tool_choice："auto"/"none"/"required"
// 透传；{type:"function", function:{name}} → {type:"function", name}。
func chatToolChoice(req map[string]any) (any, bool) {
	v, ok := req["tool_choice"]
	if !ok || v == nil {
		return nil, false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	if m, ok := v.(map[string]any); ok && m["type"] == "function" {
		if fn, ok := m["function"].(map[string]any); ok {
			if name, ok := str(fn, "name"); ok {
				return map[string]any{"type": "function", "name": name}, true
			}
		}
	}
	return nil, false
}

// respToChatResponse resp 响应对象 → chat completion 对象（非流式）。
func respToChatResponse(body []byte) ([]byte, error) {
	r, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	id, _ := str(r, "id")
	model, _ := str(r, "model")
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": intOr0(r, "created_at"),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       respToChatMessage(r),
			"finish_reason": respToChatFinishReason(r),
		}},
	}
	if u, ok := respUsageToChat(r); ok {
		out["usage"] = u
	}
	return json.Marshal(out)
}

// respToChatMessage resp output → chat assistant message（output_text 部件拼接
// content；function_call → tool_calls）。
func respToChatMessage(r map[string]any) map[string]any {
	msg := map[string]any{"role": "assistant", "content": ""}
	var text []string
	var tcs []any
	if output, ok := arr(r, "output"); ok {
		for _, item := range output {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch im["type"] {
			case "message":
				if content, ok := arr(im, "content"); ok {
					for _, part := range content {
						if pm, ok := part.(map[string]any); ok {
							if t, ok := str(pm, "text"); ok {
								text = append(text, t)
							}
						}
					}
				}
			case "function_call":
				id := toolCallID(im) // call_id 优先（客户端回传匹配键，M-1）
				name, _ := str(im, "name")
				args, _ := str(im, "arguments")
				tcs = append(tcs, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
			}
		}
	}
	msg["content"] = joinStrings(text, "")
	if len(tcs) > 0 {
		msg["tool_calls"] = tcs
	}
	return msg
}

// respToChatFinishReason resp 状态/输出 → chat finish_reason：
// incomplete → "length"；含 function_call → "tool_calls"；其余 "stop"。
func respToChatFinishReason(r map[string]any) string {
	if status, _ := str(r, "status"); status == "incomplete" {
		return "length"
	}
	if output, ok := arr(r, "output"); ok {
		for _, item := range output {
			if im, ok := item.(map[string]any); ok && im["type"] == "function_call" {
				return "tool_calls"
			}
		}
	}
	return "stop"
}

// respUsageToChat resp usage → chat usage（input/output/total 同构；
// input_tokens_details.cached_tokens → prompt_tokens_details.cached_tokens）。
func respUsageToChat(r map[string]any) (map[string]any, bool) {
	u, ok := r["usage"].(map[string]any)
	if !ok || u == nil {
		return nil, false
	}
	out := map[string]any{
		"prompt_tokens":     intOr0(u, "input_tokens"),
		"completion_tokens": intOr0(u, "output_tokens"),
		"total_tokens":      intOr0(u, "total_tokens"),
	}
	if d, ok := u["input_tokens_details"].(map[string]any); ok {
		if c := intOr0(d, "cached_tokens"); c > 0 {
			out["prompt_tokens_details"] = map[string]any{"cached_tokens": c}
		}
	}
	return out, true
}

// mapRespToChat 流式：resp SSE 事件 → chat 流。事件映射表：
//
//	response.created                  → 角色前导 chunk（delta.role=assistant）
//	response.output_text.delta        → content delta chunk
//	response.output_item.added(FC)    → tool_calls 前导 chunk（id+name）
//	response.function_call_arguments.delta → tool_calls arguments delta
//	response.completed                → 收尾 chunk（finish_reason+usage）+ [DONE]
//	response.failed                   → data-only {"error":{...}} 帧（chat 流式错误约定）
//	其余（in_progress/output_item.done/content_part.* 等）→ 丢弃
func (m *StreamMapper) mapRespToChat(name string, data []byte) ([]byte, bool) {
	ev, err := decodeObj(data)
	if err != nil {
		return nil, true
	}
	switch name {
	case "response.created":
		if m.started {
			return nil, true
		}
		m.started = true
		if resp, ok := ev["response"].(map[string]any); ok {
			m.id, _ = str(resp, "id")
			m.model, _ = str(resp, "model")
			m.created = intOr0(resp, "created_at")
		}
		return m.chatFrame(map[string]any{"role": "assistant", "content": ""}, nil, nil), false
	case "response.output_text.delta":
		delta, _ := str(ev, "delta")
		return m.chatFrame(map[string]any{"content": delta}, nil, nil), false
	case "response.output_item.added":
		item, ok := ev["item"].(map[string]any)
		if !ok || item["type"] != "function_call" {
			return nil, true
		}
		id := toolCallID(item) // call_id 优先（客户端回传匹配键，M-1）
		name, _ := str(item, "name")
		index := intOr0(ev, "output_index")
		return m.chatFrame(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": ""},
		}}}, nil, nil), false
	case "response.function_call_arguments.delta":
		delta, _ := str(ev, "delta")
		index := intOr0(ev, "output_index")
		return m.chatFrame(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "function": map[string]any{"arguments": delta},
		}}}, nil, nil), false
	case "response.completed":
		if m.done {
			return nil, true
		}
		m.done = true
		var finish any = "stop"
		var usage map[string]any
		if resp, ok := ev["response"].(map[string]any); ok {
			finish = respToChatFinishReason(resp)
			usage, _ = respUsageToChat(resp)
		}
		// 收尾 chunk：finish_reason + 内联 usage（chat 流式 include_usage 语义）
		return append(m.chatFrame(map[string]any{}, finish, usage), []byte("data: [DONE]\n\n")...), false
	case "response.failed":
		if m.done {
			return nil, true
		}
		m.done = true
		if resp, ok := ev["response"].(map[string]any); ok {
			if e, ok := resp["error"].(map[string]any); ok {
				msg, _ := str(e, "message")
				return EncodeFrame("", map[string]any{"error": map[string]any{"message": msg}}), false
			}
		}
		return []byte("data: [DONE]\n\n"), false
	}
	return nil, true
}

// chatFrame 组装 chat 流式 chunk 帧（id/object/created/model 与 delta 合并；
// finish 非 nil 时写入 finish_reason；usage 非 nil 时内联——收尾 chunk 用）。
func (m *StreamMapper) chatFrame(delta map[string]any, finish, usage any) []byte {
	c := map[string]any{
		"id":      m.id,
		"object":  "chat.completion.chunk",
		"created": m.created,
		"model":   m.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		c["usage"] = usage
	}
	return EncodeFrame("", c)
}
