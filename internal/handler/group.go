// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// PostGroups 创建分组（平台容量池；key 为独立表，用户面 /api/user/keys 创建，
// ServerInterface）。price_multiplier（正常值 0~10，API 边界换算万分数）：
// 缺省/null = 不设置（×1）；显式 0 = 免费组（恒写入——T3.5 修正：API 可表达
// 显式 0，repo 不再把 0 当未指定）。
func (h *AdminAPI) PostGroups(w http.ResponseWriter, r *http.Request) {
	var in GroupCreate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	visibility := domain.GroupVisibilityPublic
	if in.Visibility != nil {
		visibility = domain.GroupVisibility(*in.Visibility)
	}
	publicStatus := domain.GroupPublicStatusAvailable
	if in.PublicStatus != nil {
		publicStatus = domain.GroupPublicStatus(*in.PublicStatus)
		if !publicStatus.Valid() {
			httpface.WriteErr(w, http.StatusBadRequest, "invalid public status")
			return
		}
	}
	remark := ""
	if in.Remark != nil {
		var remarkErr error
		remark, remarkErr = normalizeGroupRemarkInput(*in.Remark)
		if remarkErr != nil {
			httpface.WriteErr(w, http.StatusBadRequest, remarkErr.Error())
			return
		}
	}
	category := ""
	if in.Category != nil {
		category = strings.TrimSpace(*in.Category)
	}
	mult, err := apiMultiplierToMillis(in.PriceMultiplier) // nil = 未指定；0~10 → 万分数
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// protocol_convert 方向集合（缺省 = 空数组 = off = 不转换；非法值/冲突
	// 校验在 service 层 400）
	var pcs []domain.ProtocolConvert
	if in.ProtocolConvert != nil {
		pcs = make([]domain.ProtocolConvert, 0, len(*in.ProtocolConvert))
		for _, pc := range *in.ProtocolConvert {
			pcs = append(pcs, domain.ProtocolConvert(pc))
		}
	}
	routingMode := domain.GroupRoutingModeAccounts
	if in.RoutingMode != nil {
		routingMode = domain.GroupRoutingMode(*in.RoutingMode)
	} else if in.UpstreamMembers != nil && len(*in.UpstreamMembers) > 0 {
		// The simplified admin flow only asks for the upstreams and models. Treat
		// a non-empty member list as an upstream-routed group when callers omit
		// the internal routing_mode field; explicit values still win.
		routingMode = domain.GroupRoutingModeUpstreams
	}
	allowedModels := []string(nil)
	if in.AllowedModels != nil {
		allowedModels = append([]string(nil), (*in.AllowedModels)...)
	}
	if routingMode != domain.GroupRoutingModeUpstreams && in.UpstreamMembers != nil && len(*in.UpstreamMembers) > 0 {
		httpface.WriteErr(w, http.StatusBadRequest, "upstream_members requires routing_mode=upstreams")
		return
	}
	if routingMode == domain.GroupRoutingModeUpstreams && in.UpstreamMembers != nil {
		members := make([]*domain.GroupUpstream, 0, len(*in.UpstreamMembers))
		for _, item := range *in.UpstreamMembers {
			enabled := true
			if item.Enabled != nil {
				enabled = *item.Enabled
			}
			weight, priority, maxConcurrency := 100, 0, 8
			if item.Weight != nil {
				weight = *item.Weight
			}
			if item.Priority != nil {
				priority = *item.Priority
			}
			if item.MaxConcurrency != nil {
				maxConcurrency = *item.MaxConcurrency
			}
			members = append(members, &domain.GroupUpstream{
				UpstreamID: item.UpstreamId, Weight: weight, Priority: priority,
				MaxConcurrency: maxConcurrency, Enabled: enabled,
			})
		}
		g := &domain.Group{
			Name: in.Name, Remark: remark, Category: category, Visibility: visibility, PublicStatus: publicStatus, RoutingMode: routingMode,
			AllowedModels: allowedModels, PriceMultiplier: 10000,
			ProtocolConverts: pcs,
		}
		if mult != nil {
			g.PriceMultiplier = *mult
		}
		created, createErr := h.svc.CreateUpstreamGroup(r.Context(), g, members)
		if createErr != nil {
			httpface.WriteServiceErr(w, createErr)
			return
		}
		httpface.WriteJSON(w, http.StatusOK, toAPIGroup(created))
		return
	}
	g, err := h.svc.CreateGroupWithRoutingAndRemark(r.Context(), in.Name, remark, category, visibility, mult, pcs, routingMode, allowedModels)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIGroup(g))
}

func normalizeGroupRemarkInput(remark string) (string, error) {
	remark = strings.TrimSpace(remark)
	if len([]rune(remark)) > 500 {
		return "", errors.New("remark must be 500 characters or fewer")
	}
	return remark, nil
}

// GetGroups 分组列表（分页/筛选/排序，ServerInterface）。
func (h *AdminAPI) GetGroups(w http.ResponseWriter, r *http.Request, params GetGroupsParams) {
	q := repository.ListQuery{
		Limit:  httpface.ClampLimit(int(deref(params.Limit))),
		Offset: int(deref(params.Offset)),
		Name:   deref(params.Name),
		Sort:   string(deref(params.Sort)),
		Order:  string(deref(params.Order)),
	}
	rows, total, err := h.svc.ListGroups(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]Group, 0, len(rows))
	for _, g := range rows {
		out = append(out, toAPIGroup(g))
	}
	httpface.WriteJSON(w, http.StatusOK, GroupListResponse{Total: total, Rows: out})
}

func (h *AdminAPI) PostGroupsReorder(w http.ResponseWriter, r *http.Request) {
	var in ReorderRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := h.svc.ReorderGroups(r.Context(), in.Ids); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, ReorderResponse{Reordered: len(in.Ids)})
}

// GetGroupsId 分组详情（ServerInterface）。
func (h *AdminAPI) GetGroupsId(w http.ResponseWriter, r *http.Request, id int64) {
	g, err := h.svc.GetGroup(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIGroup(g))
}

// PutGroupsId 全量更新分组（ServerInterface）。price_multiplier（正常值
// 0~10）提供（含 0 = 免费）即写入；缺省 = 保持原值。
func (h *AdminAPI) PutGroupsId(w http.ResponseWriter, r *http.Request, id int64) {
	var in GroupCreate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	g, err := h.svc.GetGroup(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	g.Name = in.Name
	if in.Remark != nil {
		remark, remarkErr := normalizeGroupRemarkInput(*in.Remark)
		if remarkErr != nil {
			httpface.WriteErr(w, http.StatusBadRequest, remarkErr.Error())
			return
		}
		g.Remark = remark
	}
	if in.Category != nil {
		g.Category = strings.TrimSpace(*in.Category)
	}
	if in.PublicStatus != nil {
		g.PublicStatus = domain.GroupPublicStatus(*in.PublicStatus)
		if !g.PublicStatus.Valid() {
			httpface.WriteErr(w, http.StatusBadRequest, "invalid public status")
			return
		}
	}
	if in.Visibility != nil {
		g.Visibility = domain.GroupVisibility(*in.Visibility)
	}
	if in.PriceMultiplier != nil {
		mult, err := apiMultiplierToMillis(in.PriceMultiplier)
		if err != nil {
			httpface.WriteErr(w, http.StatusBadRequest, err.Error())
			return
		}
		g.PriceMultiplier = *mult
	}
	// protocol_convert 缺省（null/省略）= 保持原值（读改写路径携带原值自然
	// 保留）；显式数组（含空数组 = 清空既有方向）→ 覆盖。
	if in.ProtocolConvert != nil {
		g.ProtocolConverts = nil
		for _, pc := range *in.ProtocolConvert {
			g.ProtocolConverts = append(g.ProtocolConverts, domain.ProtocolConvert(pc))
		}
	}
	if in.RoutingMode != nil {
		g.RoutingMode = domain.GroupRoutingMode(*in.RoutingMode)
	} else if in.UpstreamMembers != nil && len(*in.UpstreamMembers) > 0 {
		// Keep PUT ergonomic for API clients that use the same compact payload as
		// POST. Supplying members without an explicit mode switches the group to
		// the upstream pool atomically with the relation replacement below.
		g.RoutingMode = domain.GroupRoutingModeUpstreams
	}
	if in.AllowedModels != nil {
		g.AllowedModels = append([]string(nil), (*in.AllowedModels)...)
	}
	if in.UpstreamMembers != nil {
		if g.EffectiveRoutingMode() != domain.GroupRoutingModeUpstreams && len(*in.UpstreamMembers) > 0 {
			httpface.WriteErr(w, http.StatusBadRequest, "upstream_members requires routing_mode=upstreams")
			return
		}
		members := make([]*domain.GroupUpstream, 0, len(*in.UpstreamMembers))
		for _, item := range *in.UpstreamMembers {
			enabled := true
			if item.Enabled != nil {
				enabled = *item.Enabled
			}
			weight, priority, maxConcurrency := 100, 0, 8
			if item.Weight != nil {
				weight = *item.Weight
			}
			if item.Priority != nil {
				priority = *item.Priority
			}
			if item.MaxConcurrency != nil {
				maxConcurrency = *item.MaxConcurrency
			}
			members = append(members, &domain.GroupUpstream{
				UpstreamID: item.UpstreamId, Weight: weight, Priority: priority,
				MaxConcurrency: maxConcurrency, Enabled: enabled,
			})
		}
		updated, updateErr := h.svc.UpdateGroupWithUpstreams(r.Context(), g, members)
		if updateErr != nil {
			httpface.WriteServiceErr(w, updateErr)
			return
		}
		httpface.WriteJSON(w, http.StatusOK, toAPIGroup(updated))
		return
	}
	// Switching an upstream-routed group back to account routing must remove
	// the old relation rows even when the client omits upstream_members. Keep
	// the policy and relation replacement atomic so a subsequent switch back
	// cannot resurrect stale members.
	if in.RoutingMode != nil && g.EffectiveRoutingMode() == domain.GroupRoutingModeAccounts {
		updated, updateErr := h.svc.UpdateGroupWithUpstreams(r.Context(), g, nil)
		if updateErr != nil {
			httpface.WriteServiceErr(w, updateErr)
			return
		}
		httpface.WriteJSON(w, http.StatusOK, toAPIGroup(updated))
		return
	}
	updated, err := h.svc.UpdateGroup(r.Context(), g)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIGroup(updated))
}

// PutGroupsIdAssignments 设置组的授予用户（platform_admin 专属；替换语义：
// 未列出即撤销，空数组 = 清空；ServerInterface）。multipliers 可选：user_id →
// 该用户在该组的专属价格倍率（正常值 0~10；null = 清除为未设置 → 回退组
// 倍率；T3.5 修正：用户专属倍率按组挂载）。
func (h *AdminAPI) PutGroupsIdAssignments(w http.ResponseWriter, r *http.Request, id int64) {
	var in GroupAssignmentsBody
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var mults map[int64]*int
	if in.Multipliers != nil {
		m, err := apiMultiplierMap(*in.Multipliers) // map[string]*float64 → map[int64]*int（万分数）
		if err != nil {
			httpface.WriteErr(w, http.StatusBadRequest, err.Error())
			return
		}
		mults = m
	}
	applied, postMults, err := h.svc.SetGroupAssignments(r.Context(), id, in.UserIds, mults)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, GroupAssignmentsResponse{
		UserIds:     applied,
		Multipliers: toAPIMultipliers(postMults),
	})
}

// GetGroupsIdAssignments 读取组的授予用户与专属倍率（platform_admin；与 PUT
// 对称，供前端预填充与安全全量写回；ServerInterface）。mults 只含有专属倍率
// 的用户（null/缺省 = 未设置 → 用组倍率）。
func (h *AdminAPI) GetGroupsIdAssignments(w http.ResponseWriter, r *http.Request, id int64) {
	ids, mults, err := h.svc.GetGroupAssignments(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, GroupAssignmentsResponse{
		UserIds:     ids,
		Multipliers: toAPIMultipliers(mults),
	})
}

// DeleteGroupsId 删除分组（ServerInterface）。
func (h *AdminAPI) DeleteGroupsId(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeleteGroup(r.Context(), id); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// PostGroupsBatchDelete 批量删除分组（事务，全成或全败；key 清理由
// service 完成，ServerInterface）。
func (h *AdminAPI) PostGroupsBatchDelete(w http.ResponseWriter, r *http.Request) {
	var in BatchDeleteBody
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.DeleteGroupsBatch(r.Context(), ids); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, BatchDeleteResponse{Deleted: len(ids)})
}

// PostGroupsBatchUpdate 批量更新分组（fields 任意子集，ServerInterface）。
func (h *AdminAPI) PostGroupsBatchUpdate(w http.ResponseWriter, r *http.Request) {
	var in BatchUpdateGroupsBody
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := groupPatchFromBody(&in.Fields)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.UpdateGroupsBatch(r.Context(), ids, p); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, BatchUpdateResponse{Updated: len(ids)})
}

// groupPatchFromBody 生成类型 fields → repo patch（nil 字段 = 不更新）。
func groupPatchFromBody(f *GroupPatch) (repository.GroupPatch, error) {
	if f.Name == nil && f.Remark == nil && f.Visibility == nil && f.PublicStatus == nil {
		return repository.GroupPatch{}, errors.New("fields must contain at least one field")
	}
	p := repository.GroupPatch{Name: f.Name}
	if f.Remark != nil {
		remark, err := normalizeGroupRemarkInput(*f.Remark)
		if err != nil {
			return repository.GroupPatch{}, err
		}
		p.Remark = &remark
	}
	if f.Visibility != nil {
		v := domain.GroupVisibility(*f.Visibility)
		p.Visibility = &v
	}
	if f.PublicStatus != nil {
		v := domain.GroupPublicStatus(*f.PublicStatus)
		if !v.Valid() {
			return repository.GroupPatch{}, errors.New("invalid public status")
		}
		p.PublicStatus = &v
	}
	return p, nil
}
