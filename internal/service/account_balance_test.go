// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
)

type balanceReaderWithOneFailure struct{}

func (balanceReaderWithOneFailure) Snapshot(_ context.Context, id int64) (billing.BalanceSnapshot, error) {
	if id == 2 {
		return billing.BalanceSnapshot{}, errors.New("repository detail must stay internal")
	}
	return billing.BalanceSnapshot{AccountID: id, Provider: "relay", Status: billing.BalanceStatusFresh, Amount: "3"}, nil
}

func TestAccountsBalancesIsolatesSingleAccountFailure(t *testing.T) {
	svc := New(newFakeStore(), nil, NopInvalidator{}, nil, nil, nil, nil)
	svc.SetProviderBalanceSnapshotter(balanceReaderWithOneFailure{})

	items, err := svc.AccountsBalances(context.Background(), []int64{1, 2, 3})
	require.NoError(t, err)
	require.Len(t, items, 3)
	require.Equal(t, billing.BalanceStatusFresh, items[0].Status)
	require.Equal(t, billing.BalanceStatusUnavailable, items[1].Status)
	require.Equal(t, billing.BalanceErrorUpstream, items[1].ErrorCode)
	require.Equal(t, billing.BalanceStatusFresh, items[2].Status)
}

func TestSetProviderBalanceSnapshotterTreatsTypedNilAsUnset(t *testing.T) {
	svc := New(newFakeStore(), nil, NopInvalidator{}, nil, nil, nil, nil)
	var typedNil *balanceReaderWithOneFailure
	svc.SetProviderBalanceSnapshotter(typedNil)

	items, err := svc.AccountsBalances(context.Background(), []int64{7})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, billing.BalanceStatusUnconfigured, items[0].Status)
	require.Equal(t, billing.BalanceErrorNoEndpoint, items[0].ErrorCode)
}
