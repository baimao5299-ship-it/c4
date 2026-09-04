// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetAdminReferrals returns the complete inviter/invitee relationship audit
// view. Email addresses are resolved server-side so the admin UI can identify
// both accounts without issuing one request per row.
func (h *AdminAPI) GetAdminReferrals(w http.ResponseWriter, r *http.Request, params GetAdminReferralsParams) {
	q, err := pageToQuery(params.Page, params.PageSize)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	inviterID, inviteeID := int64(0), int64(0)
	if params.InviterId != nil {
		if *params.InviterId <= 0 {
			httpface.WriteErr(w, http.StatusBadRequest, "inviter_id must be positive")
			return
		}
		inviterID = *params.InviterId
	}
	if params.InviteeId != nil {
		if *params.InviteeId <= 0 {
			httpface.WriteErr(w, http.StatusBadRequest, "invitee_id must be positive")
			return
		}
		inviteeID = *params.InviteeId
	}
	rows, total, err := h.svc.ListReferrals(r.Context(), inviterID, inviteeID, q.Limit, q.Offset)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]ReferralRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, ReferralRecord{
			Id: row.ID, InviterId: row.InviterID, InviterEmail: row.InviterEmail,
			InviteeId: row.InviteeID, InviteeEmail: row.InviteeEmail, CreatedAt: row.CreatedAt,
		})
	}
	httpface.WriteJSON(w, http.StatusOK, ReferralRecordListResponse{Total: total, Rows: out})
}

// GetAdminBalanceLedger returns immutable balance audit rows, including the
// exact before/after snapshots and the actor that caused each change.
func (h *AdminAPI) GetAdminBalanceLedger(w http.ResponseWriter, r *http.Request, params GetAdminBalanceLedgerParams) {
	q, err := pageToQuery(params.Page, params.PageSize)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	userID := int64(0)
	if params.UserId != nil {
		if *params.UserId <= 0 {
			httpface.WriteErr(w, http.StatusBadRequest, "user_id must be positive")
			return
		}
		userID = *params.UserId
	}
	rows, total, err := h.svc.ListBalanceLedger(r.Context(), userID, q.Limit, q.Offset)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]BalanceLedgerEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAPIBalanceLedger(row))
	}
	httpface.WriteJSON(w, http.StatusOK, BalanceLedgerListResponse{Total: total, Rows: out})
}

func toAPIBalanceLedger(row *domain.BalanceLedgerEntry) BalanceLedgerEntry {
	return BalanceLedgerEntry{
		Id: row.ID, UserId: row.UserID, Kind: BalanceLedgerEntryKind(row.Kind),
		SourceId: row.SourceID, Note: row.Note, IdempotencyKey: row.IdempotencyKey,
		Delta: millisToUSD(row.Delta), BalanceBefore: millisToUSD(row.BalanceBefore),
		BalanceAfter: millisToUSD(row.BalanceAfter), ActorUserId: row.ActorUserID,
		CreatedAt: row.CreatedAt,
	}
}
