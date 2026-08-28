// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"slices"

	"github.com/is7qin/c3api/internal/domain"
)

// GroupModels 返回组快照的可路由模型列表（GET /v1/models 端点冷面数据源）：
// 账号池读取 groupSnapshot.routes，上游池读取 upstreamRoutes；键的 model
// 去重（排除空 model 的默认回退桶 routeKey{format, ""}），按 id 字典序稳定排序。快照未加载/组不存在 →
// (nil, false)——同 Select 的 ErrGroupNotFound 语义（鉴权已过但组失效 → 404）。
// 冷面路径：每次请求遍历 routes 键 + 排序，零新增常驻结构（复用调度器内存
// 快照，零 DB；与 Select 同读面——routes map 整体换入换出，无锁并发安全）。
func (s *Scheduler) GroupModels(groupID int64) ([]string, bool) {
	groups, ok := s.store.groups.Load().(map[int64]*groupSnapshot)
	if !ok {
		return nil, false
	}
	gs, ok := groups[groupID]
	if !ok {
		return nil, false
	}
	// Account pools expose model-specific route keys in routes. Upstream pools
	// keep the same information in upstreamRoutes; their default route has an
	// empty model key, so only explicit model keys are advertised here. This
	// keeps /v1/models aligned with the routes that can actually be selected.
	set := make(map[string]struct{})
	if gs.routingMode == domain.GroupRoutingModeUpstreams {
		for k := range gs.upstreamRoutes {
			if k.model != "" {
				set[k.model] = struct{}{}
			}
		}
	} else {
		for k := range gs.routes {
			if k.model != "" {
				set[k.model] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	slices.Sort(out)
	return out, true
}
