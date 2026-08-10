// Package protoconv 提供网关级协议转换（W5，只补差语义）：客户端协议请求体
// → 模板协议请求体（ConvertRequest）、模板协议响应 → 客户端协议响应
// （ConvertResponse 非流式 JSON / StreamMapper 流式 SSE 事件映射）。
//
// 边界（用户拍板）：转换器是网关能力，纯标准库（encoding/json），与 OpenAI/
// Anthropic SDK 零耦合；四方向分派按 groups.protocol_convert 快照值（off 不
// 经过本包——热路径分支在 internal/proxy 判定）；WS 帧流转换不做（resp-ws
// 1:1 透传，W3 范围）。
package protoconv

import (
	"encoding/json"
	"fmt"

	"go-proxy-mini/internal/domain"
)

// ConvertRequest 把客户端协议请求体转换为 dir 指向的模板协议请求体（返回的
// 字节即模板协议上游请求体，可直接转发）。转换器按目标协议规范映射字段；
// 目标协议无对应参数的字段（如 chat 的 frequency_penalty → resp 无此参数）
// 按规范丢弃。model 恒保持（调度器 ModelMapping 在转换后改写）。
func ConvertRequest(body []byte, dir domain.ProtocolConvert) ([]byte, error) {
	switch dir {
	case domain.ProtocolConvertChatToResp:
		return chatToRespRequest(body)
	case domain.ProtocolConvertMessToResp:
		return messToRespRequest(body)
	case domain.ProtocolConvertRespToMess:
		return respToMessRequest(body)
	case domain.ProtocolConvertChatToMess:
		return chatToMessRequest(body)
	}
	return nil, fmt.Errorf("protoconv: unsupported direction %q", dir)
}

// ConvertResponse 把模板协议的非流式响应 JSON 转换为客户端协议响应 JSON。
func ConvertResponse(body []byte, dir domain.ProtocolConvert) ([]byte, error) {
	switch dir {
	case domain.ProtocolConvertChatToResp:
		return respToChatResponse(body)
	case domain.ProtocolConvertMessToResp:
		return respToMessResponse(body)
	case domain.ProtocolConvertRespToMess:
		return messToRespResponse(body)
	case domain.ProtocolConvertChatToMess:
		return messToChatResponse(body)
	}
	return nil, fmt.Errorf("protoconv: unsupported direction %q", dir)
}

// NewStreamMapper 构造有状态的流式响应事件映射器（每 SSE 流一个实例；
// 跨事件状态：id/model、用量累积、mess→resp 的块级输出累积）。
func NewStreamMapper(dir domain.ProtocolConvert) *StreamMapper {
	return &StreamMapper{
		dir:          dir,
		blockStarted: make(map[int64]bool),
		blockStopped: make(map[int64]bool),
		textByIndex:  make(map[int64]string),
		argsByIndex:  make(map[int64]string),
		fcNames:      make(map[int64]string),
		fcIDs:        make(map[int64]string),
		blockOrder:   nil,
	}
}

// StreamMapper 按方向把模板协议的 SSE 事件映射为客户端协议帧（[]byte 即完整
// 帧字节，含 event/data 行与结尾空行）。映射帧为每次调用新分配，调用方可立即
// 写出后丢弃。
type StreamMapper struct {
	dir domain.ProtocolConvert

	started bool // 已发出首事件（防御重复的 created/start）
	done    bool // 已发出终止帧（防御重复的 completed/stop）
	id      string
	model   string
	created int64
	it, ot  int64 // 用量（input/output tokens）
	cached  int64 // cache_read / cached_tokens
	reason  string

	// mess→resp（RespToMess）方向：块级累积状态。
	blockStarted map[int64]bool
	blockStopped map[int64]bool
	textByIndex  map[int64]string
	argsByIndex  map[int64]string
	fcNames      map[int64]string
	fcIDs        map[int64]string
	blockOrder   []int64 // 内容块出现顺序（output items 保持顺序）
}

// Map 把一个模板协议 SSE 事件映射为客户端协议帧；drop=true 丢弃该帧。
// name 为空（data-only 帧）在模板协议（resp/messages）无标准用途，一律丢弃
// ——chat 的 [DONE] 等终止帧由映射器自行产出，不依赖透传。
func (m *StreamMapper) Map(name string, data []byte) ([]byte, bool) {
	if name == "" {
		return nil, true
	}
	switch m.dir {
	case domain.ProtocolConvertChatToResp:
		return m.mapRespToChat(name, data)
	case domain.ProtocolConvertMessToResp:
		return m.mapRespToMess(name, data)
	case domain.ProtocolConvertRespToMess:
		return m.mapMessToResp(name, data)
	case domain.ProtocolConvertChatToMess:
		return m.mapMessToChat(name, data)
	}
	return nil, true
}

// itemID 合成 resp output item id（message item 专用；确定性，无需随机源）。
func (m *StreamMapper) itemID(index int64) string {
	return "msg_" + m.id + "_" + fmt.Sprint(index)
}

// EncodeFrame 组装 SSE 帧字节：name 非空 → "event: name\n" 行；data 为 JSON
// 载荷（marshal 为单行 data）。载荷不可 marshal（转换器仅产出 map/string 等
// 可序列化值）→ 返回 nil，调用方按丢弃处理。
func EncodeFrame(name string, data any) []byte {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	out := make([]byte, 0, len(name)+len(payload)+16)
	if name != "" {
		out = append(out, "event: "...)
		out = append(out, name...)
		out = append(out, '\n')
	}
	out = append(out, "data: "...)
	out = append(out, payload...)
	out = append(out, '\n', '\n')
	return out
}

// decodeObj 解析 JSON 对象（转换器输入恒为对象）。
func decodeObj(body []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// str 读字符串字段。
func str(m map[string]any, key string) (string, bool) {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

// arr 读数组字段。
func arr(m map[string]any, key string) ([]any, bool) {
	if v, ok := m[key]; ok {
		if a, ok := v.([]any); ok {
			return a, true
		}
	}
	return nil, false
}

// num 读 JSON number 字段（float64 解码）。
func num(m map[string]any, key string) (float64, bool) {
	if v, ok := m[key]; ok && v != nil {
		if f, ok := v.(float64); ok {
			return f, true
		}
	}
	return 0, false
}

// intOr0 读整数字段（缺失/类型异常 → 0）。
func intOr0(m map[string]any, key string) int64 {
	if f, ok := num(m, key); ok {
		return int64(f)
	}
	return 0
}

// pass 按表透传字段：dst[key] = src[key]（仅当存在且非 nil）。
func pass(dst, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok && v != nil {
			dst[k] = v
		}
	}
}

// blockText 提取 anthropic 内容块 content（string 或 text 块数组 → 拼接文本）。
func blockText(content any) (string, bool) {
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

// marshalAny 任意值 → JSON 字符串（失败 → "{}"）；function_call arguments
// 恒为字符串，这是其唯一合法表达。
func marshalAny(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parseJSON 解析 JSON 字符串 → any；解析失败/非字符串 → 空对象。
func parseJSON(v any) any {
	if s, ok := v.(string); ok && s != "" {
		var out any
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
	}
	return map[string]any{}
}

// joinStrings 字符串数组拼接。
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}
