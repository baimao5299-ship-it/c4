// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"context"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// AccountBalanceStore is the small read-only surface needed to resolve an
// account's effective endpoint. Keeping it local avoids coupling the balance
// cache to the repository package and makes the reader easy to test.
type AccountBalanceStore interface {
	GetAccount(context.Context, int64) (*domain.Account, error)
	GetTemplate(context.Context, int64) (*domain.Template, error)
}

// AccountBalanceReader resolves an account's effective base URL and adapter,
// then delegates the network call to BalanceCache. It is deliberately request
// driven: constructing the reader never contacts a provider.
type AccountBalanceReader struct {
	store    AccountBalanceStore
	cache    *BalanceCache
	adapters map[int64]BalanceAdapter
}

func NewAccountBalanceReader(store AccountBalanceStore, cache *BalanceCache, adapters map[int64]BalanceAdapter) *AccountBalanceReader {
	copyAdapters := make(map[int64]BalanceAdapter, len(adapters))
	for templateID, adapter := range adapters {
		if !isNilBalanceAdapter(adapter) {
			copyAdapters[templateID] = adapter
		}
	}
	return &AccountBalanceReader{store: store, cache: cache, adapters: copyAdapters}
}

// Snapshot returns an explicit unconfigured state when no adapter is bound to
// the account's template. This is distinct from a real zero balance.
func (r *AccountBalanceReader) Snapshot(ctx context.Context, accountID int64) (BalanceSnapshot, error) {
	if r == nil || r.store == nil {
		return BalanceSnapshot{AccountID: accountID, Status: BalanceStatusUnavailable, ErrorCode: BalanceErrorUpstream}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	account, err := r.store.GetAccount(ctx, accountID)
	if err != nil {
		return BalanceSnapshot{}, err
	}
	if account == nil {
		return unavailableSnapshot(accountID, "", r.cacheNow(), BalanceErrorUpstream), nil
	}
	template := account.Template
	if template == nil {
		template, err = r.store.GetTemplate(ctx, account.TemplateID)
		if err != nil {
			return BalanceSnapshot{}, err
		}
		if template == nil {
			return unavailableSnapshot(accountID, "", r.cacheNow(), BalanceErrorUpstream), nil
		}
	}
	baseURL := template.BaseURL
	if account.BaseURL != nil && strings.TrimSpace(*account.BaseURL) != "" {
		baseURL = strings.TrimSpace(*account.BaseURL)
	}
	adapter := r.adapters[account.TemplateID]
	provider := ""
	if adapter != nil {
		provider = adapter.Provider()
	}
	input := BalanceAccount{
		ID: account.ID, Provider: provider, BaseURL: baseURL, UpstreamKey: account.UpstreamKey,
	}
	if r.cache == nil {
		return BalanceSnapshot{AccountID: account.ID, Provider: provider, Status: BalanceStatusUnavailable, ErrorCode: BalanceErrorUpstream}, nil
	}
	return r.cache.Get(ctx, input, adapter), nil
}

// cacheNow keeps the nil-account path deterministic in tests while avoiding a
// second network or repository dependency. A cache configured with a fake clock
// remains the source of timestamps for normal reads.
func (r *AccountBalanceReader) cacheNow() time.Time {
	if r != nil && r.cache != nil && r.cache.now != nil {
		return r.cache.now()
	}
	return time.Now()
}

// Invalidate clears all cached variants after an account key or endpoint
// change. A nil reader is intentionally a no-op for tests and disabled billing.
func (r *AccountBalanceReader) Invalidate(accountID int64) {
	if r == nil || r.cache == nil {
		return
	}
	r.cache.InvalidateAccount(accountID)
}

var _ interface {
	Snapshot(context.Context, int64) (BalanceSnapshot, error)
	Invalidate(int64)
} = (*AccountBalanceReader)(nil)
