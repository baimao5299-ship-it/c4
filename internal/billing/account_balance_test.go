// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type accountBalanceStoreFake struct {
	account  *domain.Account
	template *domain.Template
}

func (s accountBalanceStoreFake) GetAccount(context.Context, int64) (*domain.Account, error) {
	return s.account, nil
}

func (s accountBalanceStoreFake) GetTemplate(context.Context, int64) (*domain.Template, error) {
	return s.template, nil
}

type captureBalanceAdapter struct {
	fakeBalanceAdapter
	last BalanceAccount
}

func (a *captureBalanceAdapter) Provider() string { return "relay" }

func (a *captureBalanceAdapter) Fetch(ctx context.Context, account BalanceAccount) (ProviderBalance, error) {
	a.last = account
	return a.fakeBalanceAdapter.Fetch(ctx, account)
}

func TestAccountBalanceReaderUsesAccountEndpointOverride(t *testing.T) {
	clock, now := testClock()
	cache, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)
	override := "https://account.example"
	adapter := &captureBalanceAdapter{fakeBalanceAdapter: fakeBalanceAdapter{result: ProviderBalance{Amount: "12.5"}}}
	adapter.configured.Store(true)
	reader := NewAccountBalanceReader(accountBalanceStoreFake{
		account:  &domain.Account{ID: 4, TemplateID: 9, BaseURL: &override, UpstreamKey: "secret"},
		template: &domain.Template{ID: 9, BaseURL: "https://template.example"},
	}, cache, map[int64]BalanceAdapter{9: adapter})

	snapshot, err := reader.Snapshot(context.Background(), 4)
	require.NoError(t, err)
	require.Equal(t, BalanceStatusFresh, snapshot.Status)
	require.Equal(t, "12.5", snapshot.Amount)
	require.Equal(t, int64(4), adapter.last.ID)
	require.Equal(t, override, adapter.last.BaseURL)
	require.Equal(t, "secret", adapter.last.UpstreamKey)
	require.Equal(t, *clock, snapshot.CheckedAt)
}

func TestAccountBalanceReaderUnconfiguredIsNotZero(t *testing.T) {
	_, now := testClock()
	cache, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)
	reader := NewAccountBalanceReader(accountBalanceStoreFake{
		account:  &domain.Account{ID: 5, TemplateID: 10},
		template: &domain.Template{ID: 10, BaseURL: "https://template.example"},
	}, cache, nil)

	snapshot, err := reader.Snapshot(context.Background(), 5)
	require.NoError(t, err)
	require.Equal(t, BalanceStatusUnconfigured, snapshot.Status)
	require.Equal(t, BalanceErrorNoEndpoint, snapshot.ErrorCode)
	require.Empty(t, snapshot.Amount)
}

func TestAccountBalanceReaderHandlesNilStoreRows(t *testing.T) {
	_, now := testClock()
	cache, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)

	reader := NewAccountBalanceReader(accountBalanceStoreFake{}, cache, nil)
	snapshot, err := reader.Snapshot(context.Background(), 99)
	require.NoError(t, err)
	require.Equal(t, BalanceStatusUnavailable, snapshot.Status)
	require.Equal(t, BalanceErrorUpstream, snapshot.ErrorCode)
	require.Equal(t, int64(99), snapshot.AccountID)

	reader = NewAccountBalanceReader(accountBalanceStoreFake{
		account: &domain.Account{ID: 100, TemplateID: 101},
	}, cache, nil)
	snapshot, err = reader.Snapshot(context.Background(), 100)
	require.NoError(t, err)
	require.Equal(t, BalanceStatusUnavailable, snapshot.Status)
	require.Equal(t, BalanceErrorUpstream, snapshot.ErrorCode)
}

func TestAccountBalanceReaderDropsTypedNilAdapter(t *testing.T) {
	_, now := testClock()
	cache, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)
	var typedNil *fakeBalanceAdapter
	reader := NewAccountBalanceReader(accountBalanceStoreFake{
		account:  &domain.Account{ID: 102, TemplateID: 103},
		template: &domain.Template{ID: 103, BaseURL: "https://template.example"},
	}, cache, map[int64]BalanceAdapter{103: typedNil})
	snapshot, err := reader.Snapshot(context.Background(), 102)
	require.NoError(t, err)
	require.Equal(t, BalanceStatusUnconfigured, snapshot.Status)
	require.Equal(t, BalanceErrorNoEndpoint, snapshot.ErrorCode)
}

func TestAccountBalanceReaderInvalidateDoesNotNeedDatabase(t *testing.T) {
	_, now := testClock()
	cache, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)
	adapter := &captureBalanceAdapter{fakeBalanceAdapter: fakeBalanceAdapter{result: ProviderBalance{Amount: "1"}}}
	adapter.configured.Store(true)
	reader := NewAccountBalanceReader(accountBalanceStoreFake{
		account:  &domain.Account{ID: 6, TemplateID: 11},
		template: &domain.Template{ID: 11},
	}, cache, map[int64]BalanceAdapter{11: adapter})
	_, err = reader.Snapshot(context.Background(), 6)
	require.NoError(t, err)
	reader.Invalidate(6)
	adapter.result = ProviderBalance{Amount: "2"}
	_, err = reader.Snapshot(context.Background(), 6)
	require.NoError(t, err)
	require.Equal(t, 2, adapter.callCount())
}
