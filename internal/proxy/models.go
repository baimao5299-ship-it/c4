// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net/http"
	"strconv"
)

// HandleModels GET /v1/models：OpenAI 兼容模型列表（OpenAI SDK / codex 等
// 客户端用网关 key 拉取可用模型）。数据源 = 调度器内存快照（零 DB）：
// key → meta.GroupID → 组快照 routes → 模型去重排序（scheduler.GroupModels）。
// 端点冷面（非转发热路径）：鉴权通过即放行——不走计费/限流/并发门禁（只读
// 列表端点，OpenAI 语义——声明于注释）；不建新缓存/新快照。空组/无模型 →
// 空 data 数组（200 不 404）；组不存在/快照未加载 → 404（对齐 Select 的
// ErrGroupNotFound 语义——鉴权已过但组失效）。
func (p *Proxy) HandleModels(w http.ResponseWriter, r *http.Request) {
	meta, ok := p.auth.Authenticate(r)
	if !ok {
		writeErr(w, errInvalidKey)
		return
	}
	models, ok := p.sched.GroupModels(meta.GroupID)
	if !ok {
		writeErr(w, errGroupNotFound)
		return
	}
	data := make([]modelInfo, 0, len(models))
	for _, m := range models {
		data = append(data, modelInfo{
			ID: m, Object: "model", Created: 0,
			OwnedBy: strconv.FormatInt(meta.GroupID, 10),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// modelInfo OpenAI /v1/models 的 data[] 元素（OpenAI 标准 wire）：id = 模型
// 名；object = "model"（固定）；created = 0（无意义字段，OpenAI 客户端仅读
// id）；owned_by = 组 ID 字符串（组名不在快照/鉴权面，零改动取 gid——纯
// 展示字段，客户端不消费）。
type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}
