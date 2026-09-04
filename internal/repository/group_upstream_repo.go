// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql/sqlgraph"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/groupupstream"
	"github.com/is7qin/c3api/internal/ent/upstream"
)

// UpdateGroupUpstreamStatus persists the runtime breaker state without
// touching the operator's routing policy. A missing relation is treated as a
// benign stale write because the member may have been replaced while a
// request was in flight.
func (r *GroupRepo) UpdateGroupUpstreamStatus(ctx context.Context, id int64, endpoint, key string, cooldownUntil *time.Time, failureStreak int, lastError *string) error {
	if id <= 0 {
		return fmt.Errorf("invalid group upstream id")
	}
	if failureStreak < 0 {
		failureStreak = 0
	}
	// Bind the write to the endpoint/key observed by the request. A late result
	// from an old connection must not cool down a newly edited upstream.
	keyPred := upstream.UpstreamKeyEQ(key)
	if key == "" {
		keyPred = upstream.Or(upstream.UpstreamKeyIsNil(), upstream.UpstreamKeyEQ(""))
	}
	b := r.client.GroupUpstream.Update().Where(groupupstream.IDEQ(id)).
		Where(groupupstream.HasUpstreamWith(upstream.BaseURLEQ(endpoint), keyPred)).
		SetFailureStreak(failureStreak)
	if cooldownUntil == nil {
		b.ClearCooldownUntil()
	} else {
		v := *cooldownUntil
		b.SetCooldownUntil(v)
	}
	if lastError == nil || *lastError == "" {
		b.ClearLastError()
	} else {
		b.SetLastError(domain.TruncateErrMsg(*lastError))
	}
	if _, err := b.Save(ctx); err != nil {
		return err
	}
	return nil
}

// ListGroupUpstreams returns the live upstream members of a group. The upstream
// edge is eager-loaded so callers get a safe summary (the service/API never
// serializes the write-only key).
func (r *GroupRepo) ListGroupUpstreams(ctx context.Context, groupID int64) ([]*domain.GroupUpstream, error) {
	if groupID <= 0 {
		return nil, fmt.Errorf("%w: invalid group id", ErrNotFound)
	}
	rows, err := r.client.GroupUpstream.Query().
		Where(groupupstream.GroupIDEQ(groupID)).
		WithUpstream(func(q *ent.UpstreamQuery) {
			q.Where(upstream.DeletedAtIsNil())
		}).
		Order(groupupstream.ByPriority(), groupupstream.ByID()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.GroupUpstream, 0, len(rows))
	for _, row := range rows {
		// A relation pointing at a soft-deleted upstream is stale and must not
		// become a routable member. DeleteUpstream removes current relation rows;
		// this guard also covers rows retained by older versions.
		if row.Edges.Upstream == nil {
			continue
		}
		out = append(out, toDomainGroupUpstream(row))
	}
	return out, nil
}

// SetGroupUpstreams atomically replaces all members of a group. Replacing the
// relation in one transaction means a reload can only observe the old or the
// new complete set, never a partially-written pool.
func (r *GroupRepo) SetGroupUpstreams(ctx context.Context, groupID int64, members []*domain.GroupUpstream) error {
	if groupID <= 0 {
		return fmt.Errorf("%w: invalid group id", ErrNotFound)
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	// Re-check the parent and every referenced upstream on the same transaction
	// connection as the replacement. Service-level checks happen before this
	// transaction and are therefore only advisory under concurrent deletes.
	parentOK, err := tx.Group.Query().Where(group.IDEQ(groupID), group.DeletedAtIsNil()).Exist(ctx)
	if err != nil {
		return err
	}
	if !parentOK {
		return fmt.Errorf("%w: group id=%d", ErrNotFound, groupID)
	}
	if err := lockLiveGroups(ctx, r.rowLocks, tx.Group.Query, []int64{groupID}); err != nil {
		return err
	}
	if len(members) > 0 {
		ids := make([]int64, 0, len(members))
		seen := make(map[int64]struct{}, len(members))
		for _, member := range members {
			if member == nil || member.UpstreamID <= 0 {
				return fmt.Errorf("invalid upstream member")
			}
			if _, ok := seen[member.UpstreamID]; ok {
				return fmt.Errorf("duplicate upstream member")
			}
			seen[member.UpstreamID] = struct{}{}
			ids = append(ids, member.UpstreamID)
		}
		count, err := tx.Upstream.Query().Where(upstream.IDIn(ids...), upstream.DeletedAtIsNil()).Count(ctx)
		if err != nil {
			return err
		}
		if count != len(ids) {
			return fmt.Errorf("%w: upstream member missing", ErrNotFound)
		}
		if err := lockLiveUpstreams(ctx, r.rowLocks, tx.Upstream.Query, ids); err != nil {
			return err
		}
	}
	if _, err := tx.GroupUpstream.Delete().Where(groupupstream.GroupIDEQ(groupID)).Exec(ctx); err != nil {
		return err
	}
	for _, member := range members {
		if member == nil {
			return fmt.Errorf("nil upstream member")
		}
		builder := tx.GroupUpstream.Create().
			SetGroupID(groupID).
			SetUpstreamID(member.UpstreamID).
			SetWeight(member.Weight).
			SetPriority(member.Priority).
			SetMaxConcurrency(member.MaxConcurrency).
			SetEnabled(member.Enabled)
		if _, err := builder.Save(ctx); err != nil {
			if sqlgraph.IsConstraintError(err) {
				return fmt.Errorf("upstream member constraint: %w", err)
			}
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// CreateGroupWithUpstreams persists the group policy and all member rows in a
// single transaction. A failed member insert rolls back the group as well, so
// no empty compatibility group can become visible to routing or billing.
func (r *GroupRepo) CreateGroupWithUpstreams(ctx context.Context, g *domain.Group, members []*domain.GroupUpstream) (*domain.Group, error) {
	if g == nil || g.RoutingMode != domain.GroupRoutingModeUpstreams || len(members) == 0 || len(members) > 100 {
		return nil, fmt.Errorf("invalid upstream group configuration")
	}
	upstreamIDs := make([]int64, 0, len(members))
	seen := make(map[int64]struct{}, len(members))
	for _, member := range members {
		if member == nil || member.UpstreamID <= 0 {
			return nil, fmt.Errorf("invalid upstream member")
		}
		if _, exists := seen[member.UpstreamID]; exists {
			return nil, fmt.Errorf("duplicate upstream member")
		}
		seen[member.UpstreamID] = struct{}{}
		upstreamIDs = append(upstreamIDs, member.UpstreamID)
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	allowedModels := g.AllowedModels
	if allowedModels == nil {
		allowedModels = []string{}
	}
	row, err := tx.Group.Create().
		SetName(g.Name).
		SetRemark(g.Remark).
		SetCategory(g.Category).
		SetVisibility(group.Visibility(g.Visibility)).
		SetRoutingMode(group.RoutingMode(g.EffectiveRoutingMode())).
		SetPriceMultiplier(g.PriceMultiplier).
		SetProtocolConvert(protocolConvertStrings(g.ProtocolConverts)).
		SetAllowedModels(allowedModels).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, g.Name)
		}
		return nil, err
	}
	// Validate the complete member set on the same transaction connection as the
	// group insert. This closes the check-then-write race where an upstream could
	// be soft-deleted after service validation but before relation creation.
	count, err := tx.Upstream.Query().Where(upstream.IDIn(upstreamIDs...), upstream.DeletedAtIsNil()).Count(ctx)
	if err != nil {
		return nil, err
	}
	if count != len(upstreamIDs) {
		return nil, fmt.Errorf("%w: upstream member not found", ErrNotFound)
	}
	if err := lockLiveUpstreams(ctx, r.rowLocks, tx.Upstream.Query, upstreamIDs); err != nil {
		return nil, err
	}
	for _, member := range members {
		if _, err := tx.GroupUpstream.Create().
			SetGroupID(row.ID).
			SetUpstreamID(member.UpstreamID).
			SetWeight(member.Weight).
			SetPriority(member.Priority).
			SetMaxConcurrency(member.MaxConcurrency).
			SetEnabled(member.Enabled).
			Save(ctx); err != nil {
			if sqlgraph.IsConstraintError(err) {
				return nil, fmt.Errorf("upstream member constraint: %w", err)
			}
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return toDomainGroup(row), nil
}

// UpdateGroupWithUpstreams replaces the group policy and its complete upstream
// member set in one transaction. The empty member set is valid only when the
// group switches back to account routing.
func (r *GroupRepo) UpdateGroupWithUpstreams(ctx context.Context, g *domain.Group, members []*domain.GroupUpstream) (*domain.Group, error) {
	if g == nil || g.ID <= 0 || !g.EffectiveRoutingMode().Valid() || len(members) > 100 {
		return nil, fmt.Errorf("invalid upstream group configuration")
	}
	if g.EffectiveRoutingMode() == domain.GroupRoutingModeUpstreams && len(members) == 0 {
		return nil, fmt.Errorf("upstream group requires at least one member")
	}
	upstreamIDs := make([]int64, 0, len(members))
	seen := make(map[int64]struct{}, len(members))
	for _, member := range members {
		if member == nil || member.UpstreamID <= 0 {
			return nil, fmt.Errorf("invalid upstream member")
		}
		if _, exists := seen[member.UpstreamID]; exists {
			return nil, fmt.Errorf("duplicate upstream member")
		}
		seen[member.UpstreamID] = struct{}{}
		upstreamIDs = append(upstreamIDs, member.UpstreamID)
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	allowedModels := g.AllowedModels
	if allowedModels == nil {
		allowedModels = []string{}
	}
	updated := tx.Group.Update().
		Where(group.IDEQ(g.ID), group.DeletedAtIsNil()).
		SetName(g.Name).
		SetRemark(g.Remark).
		SetCategory(g.Category).
		SetVisibility(group.Visibility(g.Visibility)).
		SetPublicStatus(group.PublicStatus(g.EffectivePublicStatus())).
		SetRoutingMode(group.RoutingMode(g.EffectiveRoutingMode())).
		SetPriceMultiplier(g.PriceMultiplier).
		SetProtocolConvert(protocolConvertStrings(g.ProtocolConverts)).
		SetAllowedModels(allowedModels)
	updatedCount, err := updated.Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, g.Name)
		}
		return nil, err
	}
	if updatedCount != 1 {
		return nil, fmt.Errorf("%w: id=%d missing", ErrNotFound, g.ID)
	}
	if err := lockLiveGroups(ctx, r.rowLocks, tx.Group.Query, []int64{g.ID}); err != nil {
		return nil, err
	}
	if len(upstreamIDs) > 0 {
		count, err := tx.Upstream.Query().Where(upstream.IDIn(upstreamIDs...), upstream.DeletedAtIsNil()).Count(ctx)
		if err != nil {
			return nil, err
		}
		if count != len(upstreamIDs) {
			return nil, fmt.Errorf("%w: upstream member not found", ErrNotFound)
		}
		if err := lockLiveUpstreams(ctx, r.rowLocks, tx.Upstream.Query, upstreamIDs); err != nil {
			return nil, err
		}
	}
	if _, err := tx.GroupUpstream.Delete().Where(groupupstream.GroupIDEQ(g.ID)).Exec(ctx); err != nil {
		return nil, err
	}
	for _, member := range members {
		if _, err := tx.GroupUpstream.Create().
			SetGroupID(g.ID).
			SetUpstreamID(member.UpstreamID).
			SetWeight(member.Weight).
			SetPriority(member.Priority).
			SetMaxConcurrency(member.MaxConcurrency).
			SetEnabled(member.Enabled).
			Save(ctx); err != nil {
			if sqlgraph.IsConstraintError(err) {
				return nil, fmt.Errorf("upstream member constraint: %w", err)
			}
			return nil, err
		}
	}
	row, err := tx.Group.Get(ctx, g.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return toDomainGroup(row), nil
}
