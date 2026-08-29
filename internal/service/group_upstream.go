// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
)

var errGroupUpstreamStoreUnavailable = fmt.Errorf("group upstream management is not configured")

func (s *Service) groupUpstreamStore() (GroupUpstreamStore, error) {
	if s == nil || s.groupUpstreams == nil {
		return nil, errGroupUpstreamStoreUnavailable
	}
	return s.groupUpstreams, nil
}

// ListGroupUpstreams returns the current live relation for a group. A deleted
// group is deliberately rejected so a stale key cannot be used to route traffic.
func (s *Service) ListGroupUpstreams(ctx context.Context, groupID int64) ([]*domain.GroupUpstream, error) {
	store, err := s.groupUpstreamStore()
	if err != nil {
		return nil, err
	}
	if _, err := s.getGroupLive(ctx, groupID); err != nil {
		return nil, err
	}
	rows, err := store.ListGroupUpstreams(ctx, groupID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return rows, nil
}

// SetGroupUpstreams validates and atomically replaces the entire member set.
// Defaults are applied here rather than in the browser so every caller gets
// the same routing behavior and malformed rows never reach the database.
func (s *Service) SetGroupUpstreams(ctx context.Context, groupID int64, members []*domain.GroupUpstream) ([]*domain.GroupUpstream, error) {
	store, err := s.groupUpstreamStore()
	if err != nil {
		return nil, err
	}
	group, err := s.getGroupLive(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if len(members) > 100 {
		return nil, fmt.Errorf("%w: at most 100 upstreams per group", ErrInvalidInput)
	}
	if group.EffectiveRoutingMode() == domain.GroupRoutingModeUpstreams && len(members) == 0 {
		return nil, fmt.Errorf("%w: upstream groups require at least one member", ErrInvalidInput)
	}
	if group.EffectiveRoutingMode() != domain.GroupRoutingModeUpstreams && len(members) > 0 {
		return nil, fmt.Errorf("%w: account groups cannot contain upstream members", ErrInvalidInput)
	}
	if group.EffectiveRoutingMode() == domain.GroupRoutingModeUpstreams {
		if err := s.validateAllowedModelsForUpstreams(ctx, group.AllowedModels, members); err != nil {
			return nil, err
		}
	}
	normalized, err := s.normalizeGroupUpstreams(ctx, groupID, members)
	if err != nil {
		return nil, err
	}
	if err := store.SetGroupUpstreams(ctx, groupID, normalized); err != nil {
		return nil, mapRepoErr(err)
	}
	// Rebuild the runtime snapshot on every instance after the complete set is
	// committed. The existing invalidator is optional in unit-test stores.
	s.invalidateUpstreamConfig(ctx)
	rows, err := store.ListGroupUpstreams(ctx, groupID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return rows, nil
}

// CreateUpstreamGroup validates and commits a complete upstream-routed group
// in one repository transaction. This is the preferred creation path for the
// admin UI; the legacy create-then-attach flow remains available to callers
// using repositories without GroupRoutingStore.
func (s *Service) CreateUpstreamGroup(ctx context.Context, group *domain.Group, members []*domain.GroupUpstream) (*domain.Group, error) {
	if group == nil || group.RoutingMode != domain.GroupRoutingModeUpstreams || len(members) == 0 || len(group.AllowedModels) == 0 {
		return nil, fmt.Errorf("%w: upstream groups require members and allowed models", ErrInvalidInput)
	}
	if strings.TrimSpace(group.Name) == "" || !group.Visibility.Valid() {
		return nil, ErrInvalidInput
	}
	if group.PriceMultiplier < 0 || group.PriceMultiplier > 100000 {
		return nil, ErrInvalidInput
	}
	converts, err := normalizeProtocolConverts(group.ProtocolConverts)
	if err != nil {
		return nil, err
	}
	allowed, err := normalizeAllowedModels(group.AllowedModels)
	if err != nil || len(allowed) == 0 {
		return nil, fmt.Errorf("%w: upstream groups require at least one allowed model", ErrInvalidInput)
	}
	if err := s.validateAllowedModelsForUpstreams(ctx, allowed, members); err != nil {
		return nil, err
	}
	store, ok := s.groupRouting.(GroupRoutingStore)
	if !ok || store == nil {
		return nil, errGroupUpstreamStoreUnavailable
	}
	normalized, err := s.normalizeGroupUpstreams(ctx, 0, members)
	if err != nil {
		return nil, err
	}
	groupCopy := *group
	groupCopy.Name = strings.TrimSpace(group.Name)
	groupCopy.ProtocolConverts = converts
	groupCopy.AllowedModels = allowed
	created, err := store.CreateGroupWithUpstreams(ctx, &groupCopy, normalized)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if s.inv != nil {
		s.inv.Multipliers()
	}
	s.invalidateGroups(created.ID)
	s.publish(ctx, notify.Change{Multipliers: true, Groups: []int64{created.ID}})
	return created, nil
}

// UpdateGroupWithUpstreams commits the group policy and its complete member set
// together. It also handles switching back to account routing by clearing the
// obsolete upstream relation in the same transaction.
func (s *Service) UpdateGroupWithUpstreams(ctx context.Context, group *domain.Group, members []*domain.GroupUpstream) (*domain.Group, error) {
	if group == nil || group.ID <= 0 {
		return nil, ErrInvalidInput
	}
	normalizedGroup, err := normalizeGroupInput(group.Name, group.Visibility, &group.PriceMultiplier, group.ProtocolConverts, group.EffectiveRoutingMode(), group.AllowedModels)
	if err != nil {
		return nil, err
	}
	normalizedGroup.ID = group.ID
	if normalizedGroup.RoutingMode == domain.GroupRoutingModeUpstreams {
		if len(members) == 0 || len(normalizedGroup.AllowedModels) == 0 {
			return nil, fmt.Errorf("%w: upstream groups require members and allowed models", ErrInvalidInput)
		}
		if err := s.validateAllowedModelsForUpstreams(ctx, normalizedGroup.AllowedModels, members); err != nil {
			return nil, err
		}
		members, err = s.normalizeGroupUpstreams(ctx, group.ID, members)
		if err != nil {
			return nil, err
		}
	} else {
		if len(members) != 0 {
			return nil, fmt.Errorf("%w: account groups cannot contain upstream members", ErrInvalidInput)
		}
		members = nil
	}
	store, ok := s.groupRouting.(GroupRoutingStore)
	if !ok || store == nil {
		return nil, errGroupUpstreamStoreUnavailable
	}
	updated, err := store.UpdateGroupWithUpstreams(ctx, normalizedGroup, members)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if s.inv != nil {
		s.inv.Multipliers()
	}
	s.invalidateGroups(updated.ID)
	s.publish(ctx, notify.Change{Multipliers: true, Keys: true, Groups: []int64{updated.ID}})
	return updated, nil
}

// validateAllowedModelsForUpstreams prevents a group from being persisted with
// an allowlist that no selected upstream can serve. A nil ModelsCheckedAt means
// the endpoint has not produced a successful catalogue yet, so its capabilities
// remain unknown and the model is accepted for compatibility. Once a catalogue
// is confirmed, the recorded list is authoritative until the next successful
// refresh; a transient refresh error keeps that last known list intact.
func (s *Service) validateAllowedModelsForUpstreams(ctx context.Context, allowed []string, members []*domain.GroupUpstream) error {
	if len(allowed) == 0 {
		return nil
	}
	if s == nil || s.upstreams == nil {
		return errUpstreamStoreUnavailable
	}
	covered := make(map[string]bool, len(allowed))
	for _, model := range allowed {
		covered[model] = false
	}
	for _, member := range members {
		if member == nil || member.UpstreamID <= 0 {
			continue
		}
		u, err := s.upstreams.GetUpstream(ctx, member.UpstreamID)
		if err != nil {
			return mapRepoErr(err)
		}
		if u == nil || u.DeletedAt != nil {
			return fmt.Errorf("%w: upstream id=%d", ErrNotFound, member.UpstreamID)
		}
		// An unchecked catalogue is deliberately treated as unknown rather than
		// as an empty list. The first successful model read will tighten routing.
		if u.ModelsCheckedAt == nil {
			for model := range covered {
				covered[model] = true
			}
			continue
		}
		for _, model := range u.Models {
			if _, ok := covered[model]; ok {
				covered[model] = true
			}
		}
	}
	for model, ok := range covered {
		if !ok {
			return fmt.Errorf("%w: no selected upstream supports model %q", ErrInvalidInput, model)
		}
	}
	return nil
}

func (s *Service) normalizeGroupUpstreams(ctx context.Context, groupID int64, members []*domain.GroupUpstream) ([]*domain.GroupUpstream, error) {
	if len(members) > 100 {
		return nil, fmt.Errorf("%w: at most 100 upstreams per group", ErrInvalidInput)
	}
	seen := make(map[int64]struct{}, len(members))
	normalized := make([]*domain.GroupUpstream, 0, len(members))
	for _, member := range members {
		if member == nil || member.UpstreamID <= 0 {
			return nil, ErrInvalidInput
		}
		if _, exists := seen[member.UpstreamID]; exists {
			return nil, fmt.Errorf("%w: duplicate upstream id %d", ErrInvalidInput, member.UpstreamID)
		}
		seen[member.UpstreamID] = struct{}{}
		if s.upstreams == nil {
			return nil, errUpstreamStoreUnavailable
		}
		u, getErr := s.upstreams.GetUpstream(ctx, member.UpstreamID)
		if getErr != nil {
			return nil, mapRepoErr(getErr)
		}
		if u == nil || u.DeletedAt != nil {
			return nil, fmt.Errorf("%w: upstream id=%d", ErrNotFound, member.UpstreamID)
		}
		cp := *member
		cp.ID = 0
		if groupID > 0 {
			cp.GroupID = groupID
		}
		if cp.Weight == 0 {
			cp.Weight = 100
		}
		if cp.MaxConcurrency == 0 {
			cp.MaxConcurrency = 8
		}
		if cp.Weight < 1 || cp.Weight > 10000 || cp.Priority < 0 || cp.Priority > 100000 || cp.MaxConcurrency < 1 || cp.MaxConcurrency > 100000 {
			return nil, ErrInvalidInput
		}
		cp.Upstream = nil
		normalized = append(normalized, &cp)
	}
	return normalized, nil
}
