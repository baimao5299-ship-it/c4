// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetGroupsIdUpstreams returns the group policy and its live upstream members.
// Upstream credentials are never part of the response projection.
func (h *AdminAPI) GetGroupsIdUpstreams(w http.ResponseWriter, r *http.Request, id int64) {
	members, err := h.svc.ListGroupUpstreams(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	g, err := h.svc.GetGroup(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	mode := GroupRoutingMode(g.EffectiveRoutingMode())
	allowed := append([]string(nil), g.AllowedModels...)
	if allowed == nil {
		allowed = []string{}
	}
	out := make([]GroupUpstream, 0, len(members))
	for _, member := range members {
		out = append(out, toAPIGroupUpstream(member))
	}
	httpface.WriteJSON(w, http.StatusOK, GroupUpstreamsResponse{
		GroupId: id, RoutingMode: mode, AllowedModels: allowed, Members: out,
	})
}

// PutGroupsIdUpstreams atomically replaces a group's upstream membership. The
// service validates IDs, deduplicates members, applies defaults and verifies
// every referenced upstream before the repository transaction starts.
func (h *AdminAPI) PutGroupsIdUpstreams(w http.ResponseWriter, r *http.Request, id int64) {
	var in GroupUpstreamsUpdate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if in.Members == nil {
		httpface.WriteErr(w, http.StatusBadRequest, "members is required (use [] to clear the pool)")
		return
	}
	members := make([]*domain.GroupUpstream, 0, len(in.Members))
	for _, item := range in.Members {
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
			GroupID: id, UpstreamID: item.UpstreamId, Weight: weight,
			Priority: priority, MaxConcurrency: maxConcurrency, Enabled: enabled,
		})
	}
	applied, err := h.svc.SetGroupUpstreams(r.Context(), id, members)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	g, err := h.svc.GetGroup(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	allowed := append([]string(nil), g.AllowedModels...)
	if allowed == nil {
		allowed = []string{}
	}
	out := make([]GroupUpstream, 0, len(applied))
	for _, member := range applied {
		out = append(out, toAPIGroupUpstream(member))
	}
	httpface.WriteJSON(w, http.StatusOK, GroupUpstreamsResponse{
		GroupId: id, RoutingMode: GroupRoutingMode(g.EffectiveRoutingMode()),
		AllowedModels: allowed, Members: out,
	})
}

func toAPIGroupUpstream(m *domain.GroupUpstream) GroupUpstream {
	if m == nil {
		return GroupUpstream{}
	}
	var upstreamView *Upstream
	if m.Upstream != nil {
		v := toAPIUpstream(m.Upstream)
		upstreamView = &v
	}
	return GroupUpstream{
		ID: m.ID, GroupID: m.GroupID, UpstreamID: m.UpstreamID,
		Upstream: upstreamView, Weight: m.Weight,
		Priority: m.Priority, MaxConcurrency: m.MaxConcurrency,
		Enabled: m.Enabled, CooldownUntil: m.CooldownUntil,
		FailureStreak: m.FailureStreak, LastError: m.LastError,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}
