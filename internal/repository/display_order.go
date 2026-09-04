// SPDX-License-Identifier: AGPL-3.0-or-later

package repository

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/upstream"
)

const displayOrderGap int64 = 1_000_000

// displayOrderOptions keeps untouched rows in the historical newest-first ID
// order. Dragged rows receive the sparse rank of the slot they occupy, so one
// page can be reordered without rewriting unrelated rows.
func displayOrderOptions(orderField, idField, direction string) (func(*sql.Selector), func(*sql.Selector)) {
	descending := strings.EqualFold(direction, "desc")
	return (func(s *sql.Selector) {
			s.OrderExprFunc(func(b *sql.Builder) {
				b.WriteString("COALESCE(").Ident(s.C(orderField)).WriteString(", -").Ident(s.C(idField)).WriteString(" * ").WriteString(fmt.Sprint(displayOrderGap)).WriteString(")")
				if descending {
					b.WriteString(" DESC")
				} else {
					b.WriteString(" ASC")
				}
			})
		}), (func(s *sql.Selector) {
			if descending {
				s.OrderBy(sql.Asc(s.C(idField)))
			} else {
				s.OrderBy(sql.Desc(s.C(idField)))
			}
		})
}

type displaySlot struct {
	id   int64
	rank int64
}

func effectiveDisplayOrder(id int64, stored *int64) int64 {
	if stored != nil {
		return *stored
	}
	return -id * displayOrderGap
}

func reorderedRanks(ids []int64, current map[int64]*int64) (map[int64]int64, error) {
	if len(ids) < 2 || len(ids) > 200 {
		return nil, fmt.Errorf("reorder requires 2 to 200 ids")
	}
	seen := make(map[int64]struct{}, len(ids))
	slots := make([]displaySlot, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("invalid reorder id")
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("duplicate reorder id")
		}
		seen[id] = struct{}{}
		stored, ok := current[id]
		if !ok {
			return nil, fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
		}
		slots = append(slots, displaySlot{id: id, rank: effectiveDisplayOrder(id, stored)})
	}
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].rank != slots[j].rank {
			return slots[i].rank < slots[j].rank
		}
		return slots[i].id > slots[j].id
	})
	out := make(map[int64]int64, len(ids))
	for i, id := range ids {
		out[id] = slots[i].rank
	}
	return out, nil
}

func (r *UpstreamRepo) ReorderUpstreams(ctx context.Context, ids []int64) error {
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
	if err := lockLiveUpstreams(ctx, r.rowLocks, tx.Upstream.Query, ids); err != nil {
		return err
	}
	rows, err := tx.Upstream.Query().Where(upstream.IDIn(ids...), upstream.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return err
	}
	current := make(map[int64]*int64, len(rows))
	for _, row := range rows {
		current[row.ID] = row.DisplayOrder
	}
	ranks, err := reorderedRanks(ids, current)
	if err != nil {
		return err
	}
	writeIDs := append([]int64(nil), ids...)
	sort.Slice(writeIDs, func(i, j int) bool { return writeIDs[i] < writeIDs[j] })
	for _, id := range writeIDs {
		rank := ranks[id]
		if _, err := tx.Upstream.UpdateOneID(id).Where(upstream.DeletedAtIsNil()).SetDisplayOrder(rank).Save(ctx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (r *GroupRepo) ReorderGroups(ctx context.Context, ids []int64) error {
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
	if err := lockLiveGroups(ctx, r.rowLocks, tx.Group.Query, ids); err != nil {
		return err
	}
	rows, err := tx.Group.Query().Where(group.IDIn(ids...), group.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return err
	}
	current := make(map[int64]*int64, len(rows))
	for _, row := range rows {
		current[row.ID] = row.DisplayOrder
	}
	ranks, err := reorderedRanks(ids, current)
	if err != nil {
		return err
	}
	writeIDs := append([]int64(nil), ids...)
	sort.Slice(writeIDs, func(i, j int) bool { return writeIDs[i] < writeIDs[j] })
	for _, id := range writeIDs {
		rank := ranks[id]
		if _, err := tx.Group.UpdateOneID(id).Where(group.DeletedAtIsNil()).SetDisplayOrder(rank).Save(ctx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}
