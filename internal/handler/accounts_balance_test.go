// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/service"
)

type balanceSnapshotStub struct{}

func (balanceSnapshotStub) Snapshot(_ context.Context, id int64) (billing.BalanceSnapshot, error) {
	return billing.BalanceSnapshot{AccountID: id, Provider: "relay", Status: billing.BalanceStatusFresh, Amount: "0", Currency: "USD", Low: true}, nil
}

type refreshableBalanceStub struct{ invalidated []int64 }

func (s *refreshableBalanceStub) Snapshot(_ context.Context, id int64) (billing.BalanceSnapshot, error) {
	return billing.BalanceSnapshot{AccountID: id, Provider: "relay", Status: billing.BalanceStatusFresh, Amount: "1", Currency: "USD"}, nil
}

func (s *refreshableBalanceStub) Invalidate(id int64) { s.invalidated = append(s.invalidated, id) }

func getBalances(h *AdminAPI, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/accounts/balances?"+query, nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	return rec
}

func TestGetAccountsBalancesValidationAndOrdering(t *testing.T) {
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	svc.SetProviderBalanceSnapshotter(balanceSnapshotStub{})
	h := New(svc)

	rec := getBalances(h, "account_ids=3,1,3")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body AccountsBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 2)
	require.Equal(t, int64(3), body.Items[0].AccountId)
	require.Equal(t, int64(1), body.Items[1].AccountId)
	require.Equal(t, "0", *body.Items[0].Amount)
	require.NotContains(t, rec.Body.String(), "0001-01-01")

	for _, query := range []string{"", "account_ids=x", "account_ids=0", "account_ids=-1", "account_ids=1,0", "account_ids=1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47,48,49,50,51,52,53,54,55,56,57,58,59,60,61,62,63,64,65,66,67,68,69,70,71,72,73,74,75,76,77,78,79,80,81,82,83,84,85,86,87,88,89,90,91,92,93,94,95,96,97,98,99,100,101"} {
		require.Equal(t, http.StatusBadRequest, getBalances(h, query).Code)
	}
}

func TestGetAccountsBalancesRefreshInvalidatesBeforeRead(t *testing.T) {
	stub := &refreshableBalanceStub{}
	svc := service.New(newFakeStore(), fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	svc.SetProviderBalanceSnapshotter(stub)
	h := New(svc)
	rec := getBalances(h, "account_ids=3,1,3&refresh=true")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []int64{3, 1}, stub.invalidated)
}

func TestGetAccountsBalancesMissingReaderIsExplicit(t *testing.T) {
	svc := service.New(newFakeStore(), fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc)
	rec := getBalances(h, "account_ids=9")
	require.Equal(t, http.StatusOK, rec.Code)
	var body AccountsBalancesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, ProviderBalanceSnapshotStatus("unconfigured"), body.Items[0].Status)
	require.Nil(t, body.Items[0].Amount)
	require.Nil(t, body.Items[0].CheckedAt)
	require.NotContains(t, rec.Body.String(), time.Time{}.Format(time.RFC3339))
}
