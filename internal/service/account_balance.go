// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"reflect"

	"golang.org/x/sync/errgroup"

	"github.com/is7qin/c3api/internal/billing"
)

// ProviderBalanceSnapshotter is the optional provider balance read path. It is
// separate from Codex usage because an API-key relay's balance endpoint and a
// Codex subscription quota are different measurements.
type ProviderBalanceSnapshotter interface {
	Snapshot(context.Context, int64) (billing.BalanceSnapshot, error)
}

type ProviderBalanceInvalidator interface {
	Invalidate(int64)
}

// SetProviderBalanceSnapshotter wires the on-demand provider balance reader.
// A nil value leaves the endpoint available but returns unconfigured items.
func (s *Service) SetProviderBalanceSnapshotter(reader ProviderBalanceSnapshotter) {
	if isNilProviderBalanceSnapshotter(reader) {
		reader = nil
	}
	s.balanceMu.Lock()
	s.providerBalances = reader
	s.balanceMu.Unlock()
}

func isNilProviderBalanceSnapshotter(reader ProviderBalanceSnapshotter) bool {
	if reader == nil {
		return true
	}
	v := reflect.ValueOf(reader)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (s *Service) invalidateProviderBalance(accountID int64) {
	s.balanceMu.RLock()
	reader := s.providerBalances
	s.balanceMu.RUnlock()
	if invalidator, ok := reader.(ProviderBalanceInvalidator); ok {
		invalidator.Invalidate(accountID)
	}
}

// InvalidateProviderBalances forces the next read for each account to contact
// the configured provider. It is intentionally explicit and request-driven so
// normal dashboard polling keeps using the bounded cache.
func (s *Service) InvalidateProviderBalances(ids []int64) {
	for _, id := range ids {
		if id > 0 {
			s.invalidateProviderBalance(id)
		}
	}
}

// AccountsBalances resolves a bounded batch concurrently and preserves the
// caller's de-duplicated order. A missing reader is represented explicitly as
// unconfigured instead of being mistaken for a zero balance.
func (s *Service) AccountsBalances(ctx context.Context, ids []int64) ([]billing.BalanceSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	out := make([]billing.BalanceSnapshot, len(ids))
	s.balanceMu.RLock()
	reader := s.providerBalances
	s.balanceMu.RUnlock()
	if reader == nil {
		for i, id := range ids {
			out[i] = billing.BalanceSnapshot{AccountID: id, Status: billing.BalanceStatusUnconfigured, ErrorCode: billing.BalanceErrorNoEndpoint}
		}
		return out, nil
	}
	var g errgroup.Group
	g.SetLimit(8)
	for i, id := range ids {
		i, id := i, id
		g.Go(func() error {
			snapshot, err := reader.Snapshot(ctx, id)
			if err != nil {
				// A single deleted or temporarily unreadable account must not make
				// the whole dashboard unusable. Keep the failure bounded to this
				// item and never expose repository or upstream error text.
				out[i] = billing.BalanceSnapshot{AccountID: id, Status: billing.BalanceStatusUnavailable, ErrorCode: billing.BalanceErrorUpstream}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return nil
			}
			out[i] = snapshot
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}
