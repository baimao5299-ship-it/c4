package proxy

import (
	"bytes"
	"encoding/json"

	"github.com/tidwall/gjson"
)

// stripImageTools 剥离 response.create 帧中的图像工具（模板级开关开启时调用；
// 调用方负责开关分支——关闭 = 快照布尔读 + 不调用，零开销）。
//
// 热路径纪律（架构定稿 §5）：本函数以 "image" 子串预筛——无命中直接返回原
// 切片（零解析零分配）；命中才最小解析（仅顶层 tools/tool_choice 键，工具
// 体 RawMessage 字节保留、不改写）。解析异常（tools 非数组等）→ 返回原帧
// 原样转发（best-effort：上游按现状拒绝，行为不变）。
//
// 边界（架构定稿）：只做 tools 数组 + tool_choice 悬挂；input 内嵌 v1 图像
// 内容不做；只服务 resp 协议（chat/messages 不做；W5 转换后为 resp 形态也
// 覆盖）。剥离在客户端入站帧转发上游前执行（网关能力，不依赖 SDK）——未来
// resp-ws 帧流（response.create 帧）复用本函数。
//
// 图像工具形态（真实 Responses API / codex 客户端，codex-rs 实证）：
//   - {"type":"image_generation_tool","namespace":"image_gen",...}（hosted 图像生成）
//   - {"type":"image_edits",...}（图像编辑）
//   - {"type":"namespace","name":"image_gen",...}（codex standalone namespace
//     工具 image_gen.imagegen——按 namespace/name == "image_gen" 判定）
//
// tool_choice 悬挂：对象形 tool_choice 的 type/name/namespace 指向已剥工具 →
// 删除 tool_choice 字段。Responses API 缺省 tool_choice = "auto"，移除 = 最简
// 正确语义（保留 "none"/"required"/"auto" 字符串形恒非悬挂）。
func stripImageTools(body []byte) []byte {
	if !bytes.Contains(body, []byte("image")) {
		return body // 预筛：无命中零解析零分配直转
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body // 解析异常：原样转发（handleFormat 已 json.Valid，理论上不可达）
	}
	toolsRaw, ok := m["tools"]
	if !ok || !bytes.HasPrefix(toolsRaw, []byte{'['}) {
		return body // 无 tools 数组：无工具可剥（input 内嵌 "image" 字样不受影响——边界）
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(toolsRaw, &tools); err != nil {
		return body // tools 非数组（string/number/object）：原样转发，上游按现状拒绝
	}
	stripped := make(map[string]struct{}, 2) // 已剥工具标识（type/name/namespace），tool_choice 悬挂判定用
	out := tools[:0]                         // 复用 Unmarshal 分配的底层数组（最少分配）
	for _, t := range tools {
		if isImageTool(t) {
			collectToolIDs(t, stripped)
			continue
		}
		out = append(out, t)
	}
	if len(out) == len(tools) {
		return body // 无图像工具（"image" 命中在 input/描述等位置）：零改动原样直转
	}
	newTools, err := json.Marshal(out)
	if err != nil {
		return body
	}
	m["tools"] = newTools
	if tc, ok := m["tool_choice"]; ok && toolChoiceDangles(tc, stripped) {
		delete(m, "tool_choice") // 悬挂 → 移除（缺省 = "auto"）
	}
	res, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return res
}

// isImageTool 判定工具对象是否为图像工具（按真实 Responses API / codex 客户端
// 形态）：type ∈ {image_generation_tool, image_edits}；或 namespace 工具且
// namespace/name == "image_gen"（codex standalone namespace 工具
// image_gen.imagegen）。type 字段恒为小写 —— 预筛 "image" 子串必然命中。
func isImageTool(t json.RawMessage) bool {
	switch gjson.GetBytes(t, "type").String() {
	case "image_generation_tool", "image_edits":
		return true
	case "namespace":
		ns := gjson.GetBytes(t, "namespace").String()
		if ns == "" {
			ns = gjson.GetBytes(t, "name").String()
		}
		return ns == "image_gen"
	}
	return false
}

// collectToolIDs 收集已剥工具的标识（type/name/namespace，非空才收录）——
// tool_choice 悬挂判定集合。
func collectToolIDs(t json.RawMessage, ids map[string]struct{}) {
	for _, k := range []string{"type", "name", "namespace"} {
		if v := gjson.GetBytes(t, k).String(); v != "" {
			ids[v] = struct{}{}
		}
	}
}

// toolChoiceDangles 判定 tool_choice 是否悬挂（指向已剥工具）：仅对象形
// （首字符 '{'）——type/name/namespace ∈ 已剥工具标识集合 → 悬挂。字符串形
// （"auto"/"none"/"required"）恒非悬挂。
func toolChoiceDangles(tc json.RawMessage, stripped map[string]struct{}) bool {
	if len(tc) == 0 || tc[0] != '{' {
		return false
	}
	for _, k := range []string{"type", "name", "namespace"} {
		if _, ok := stripped[gjson.GetBytes(tc, k).String()]; ok {
			return true
		}
	}
	return false
}
