// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetAccountsBalances returns provider balances on demand. The response keeps
// unconfigured, unavailable, stale and fresh states distinct; no credentials or
// upstream error text are exposed.
func (h *AdminAPI) GetAccountsBalances(w http.ResponseWriter, r *http.Request, params GetAccountsBalancesParams) {
	ids, err := parseAccountIDs(params.AccountIds)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if params.Refresh != nil && *params.Refresh {
		h.svc.InvalidateProviderBalances(ids)
	}
	items, err := h.svc.AccountsBalances(r.Context(), ids)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]ProviderBalanceSnapshot, 0, len(items))
	for _, item := range items {
		out = append(out, toAPIProviderBalanceSnapshot(item))
	}
	httpface.WriteJSON(w, http.StatusOK, AccountsBalancesResponse{Items: out})
}

func toAPIProviderBalanceSnapshot(item billing.BalanceSnapshot) ProviderBalanceSnapshot {
	out := ProviderBalanceSnapshot{
		AccountId: item.AccountID,
		Provider:  optionalStringValue(item.Provider),
		Status:    ProviderBalanceSnapshotStatus(item.Status),
		Amount:    optionalStringValue(item.Amount),
		Currency:  optionalStringValue(item.Currency),
		Low:       item.Low,
		ErrorCode: optionalBalanceError(item.ErrorCode),
	}
	if !item.CheckedAt.IsZero() {
		out.CheckedAt = &item.CheckedAt
	}
	if !item.AttemptedAt.IsZero() {
		out.AttemptedAt = &item.AttemptedAt
	}
	if !item.ExpiresAt.IsZero() {
		out.ExpiresAt = &item.ExpiresAt
	}
	if !item.StaleUntil.IsZero() {
		out.StaleUntil = &item.StaleUntil
	}
	return out
}

func optionalStringValue(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalBalanceError(value billing.BalanceErrorCode) *ProviderBalanceSnapshotErrorCode {
	if value == "" {
		return nil
	}
	converted := ProviderBalanceSnapshotErrorCode(value)
	return &converted
}
