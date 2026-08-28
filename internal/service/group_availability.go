// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
)

// ErrGroupUnavailable prevents issuing a key for an upstream group that has no
// route even though its visibility would otherwise allow the user to select it.
var ErrGroupUnavailable = fmt.Errorf("%w: group has no routable upstream", ErrConflict)

// ensureGroupRoutable checks persistent routing configuration only. Runtime
// cooldowns are deliberately ignored: a transient upstream failure must not
// hide a group or block key creation while the pool is recovering.
func (s *Service) ensureGroupRoutable(ctx context.Context, group *domain.Group) error {
	if group == nil || group.EffectiveRoutingMode() != domain.GroupRoutingModeUpstreams {
		return nil
	}
	store, err := s.groupUpstreamStore()
	if err != nil {
		return err
	}
	members, err := store.ListGroupUpstreams(ctx, group.ID)
	if err != nil {
		return mapRepoErr(err)
	}
	if upstreamGroupHasRoute(group.AllowedModels, members) {
		return nil
	}
	return ErrGroupUnavailable
}

// upstreamGroupHasRoute mirrors the scheduler's persistent route publication
// rules without consulting mutable runtime state. An unchecked model catalogue
// remains compatible with legacy upstreams; once checked, only recorded models
// can make a route available.
func upstreamGroupHasRoute(allowed []string, members []*domain.GroupUpstream) bool {
	live := make([]*domain.Upstream, 0, len(members))
	for _, member := range members {
		if member == nil || member.ID <= 0 || !member.Enabled || member.Upstream == nil {
			continue
		}
		u := member.Upstream
		if u.DeletedAt != nil || !u.Enabled || strings.TrimSpace(u.BaseURL) == "" {
			continue
		}
		live = append(live, u)
	}
	if len(live) == 0 {
		return false
	}

	if len(allowed) > 0 {
		for _, model := range allowed {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			for _, u := range live {
				if u.ModelsCheckedAt == nil || groupUpstreamContainsModel(u.Models, model) {
					return true
				}
			}
		}
		return false
	}

	// Legacy groups without an explicit allowlist publish an unrestricted route
	// only while every live member is unchecked. Once a catalogue exists, the
	// scheduler publishes the confirmed intersection instead.
	var intersection map[string]struct{}
	allUnchecked := true
	for _, u := range live {
		if u.ModelsCheckedAt == nil {
			continue
		}
		allUnchecked = false
		local := make(map[string]struct{}, len(u.Models))
		for _, model := range u.Models {
			if model = strings.TrimSpace(model); model != "" {
				local[model] = struct{}{}
			}
		}
		if intersection == nil {
			intersection = local
			continue
		}
		for model := range intersection {
			if _, ok := local[model]; !ok {
				delete(intersection, model)
			}
		}
	}
	return allUnchecked || len(intersection) > 0
}

func groupUpstreamContainsModel(models []string, want string) bool {
	for _, model := range models {
		if model == want {
			return true
		}
	}
	return false
}
