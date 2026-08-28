// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"sort"

	"entgo.io/ent/dialect/sql"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/template"
	"github.com/is7qin/c3api/internal/ent/upstream"
)

// These helpers are used only on a transaction client. The row lock is the
// serialization point shared by delete and relation writes; service-level
// existence checks remain useful for fast validation but cannot close a race.
// IDs are sorted to keep multi-row mutations from deadlocking each other.
func lockLiveAccounts(ctx context.Context, enabled bool, q func() *ent.AccountQuery, ids []int64) error {
	if !enabled {
		return nil
	}
	return lockLiveIDs(ctx, ids, func(chunk []int64) ([]int64, error) {
		return q().Where(account.IDIn(chunk...), account.DeletedAtIsNil(), func(s *sql.Selector) { s.ForUpdate() }).IDs(ctx)
	})
}

func lockLiveGroups(ctx context.Context, enabled bool, q func() *ent.GroupQuery, ids []int64) error {
	if !enabled {
		return nil
	}
	return lockLiveIDs(ctx, ids, func(chunk []int64) ([]int64, error) {
		return q().Where(group.IDIn(chunk...), group.DeletedAtIsNil(), func(s *sql.Selector) { s.ForUpdate() }).IDs(ctx)
	})
}

func lockLiveUpstreams(ctx context.Context, enabled bool, q func() *ent.UpstreamQuery, ids []int64) error {
	if !enabled {
		return nil
	}
	return lockLiveIDs(ctx, ids, func(chunk []int64) ([]int64, error) {
		return q().Where(upstream.IDIn(chunk...), upstream.DeletedAtIsNil(), func(s *sql.Selector) { s.ForUpdate() }).IDs(ctx)
	})
}

func lockLiveTemplates(ctx context.Context, enabled bool, q func() *ent.TemplateQuery, ids []int64) error {
	if !enabled {
		return nil
	}
	return lockLiveIDs(ctx, ids, func(chunk []int64) ([]int64, error) {
		return q().Where(template.IDIn(chunk...), template.DeletedAtIsNil(), func(s *sql.Selector) { s.ForUpdate() }).IDs(ctx)
	})
}

func lockLiveIDs(ctx context.Context, ids []int64, each func([]int64) ([]int64, error)) error {
	if len(ids) == 0 {
		return nil
	}
	ordered := append([]int64(nil), ids...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	existing := make([]int64, 0, len(ordered))
	seen := make(map[int64]struct{}, len(ordered))
	for _, id := range ordered {
		if id <= 0 {
			return diffMissing(existing, []int64{id})
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		// Lock one row per statement so the lock order is explicit even when
		// PostgreSQL chooses a different plan for a large replacement set.
		got, err := each([]int64{id})
		if err != nil {
			return err
		}
		existing = append(existing, got...)
	}
	return diffMissing(existing, ordered)
}
