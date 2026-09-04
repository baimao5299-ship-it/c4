// SPDX-License-Identifier: AGPL-3.0-or-later

package handler

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// PostUsersIdBalance performs an audited atomic credit. It intentionally does
// not reuse the absolute-balance update endpoint, so concurrent usage cannot be
// overwritten and referral rewards share the same transaction as the credit.
func (h *AdminAPI) PostUsersIdBalance(w http.ResponseWriter, r *http.Request, id int64) {
	var in BalanceAdjustmentRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	amount, err := usdToMillisChecked(in.Amount)
	if err != nil || amount <= 0 {
		httpface.WriteErr(w, http.StatusBadRequest, "amount must be positive")
		return
	}
	sourceID := uuid.NewString()
	if in.IdempotencyKey != nil {
		sourceID = strings.TrimSpace(*in.IdempotencyKey)
		if len(sourceID) < 8 || len(sourceID) > 100 {
			httpface.WriteErr(w, http.StatusBadRequest, "idempotency_key must contain 8-100 characters")
			return
		}
	}
	note := ""
	if in.Note != nil {
		note = *in.Note
	}
	if _, err := h.svc.CreditUserBalanceWithNote(r.Context(), id, amount, createdBy(r), sourceID, note); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	updated, err := h.svc.GetUser(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUser(updated))
}
