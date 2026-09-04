// SPDX-License-Identifier: AGPL-3.0-or-later

package user

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

func (h *UserAPI) GetUserReferrals(w http.ResponseWriter, r *http.Request) {
	h.writeReferralSummary(w, r)
}

func (h *UserAPI) PostUserReferralsClaim(w http.ResponseWriter, r *http.Request) {
	var in ReferralClaimRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	requestID := strings.TrimSpace(in.RequestId)
	if len(requestID) < 8 || len(requestID) > 100 {
		httpface.WriteErr(w, http.StatusBadRequest, "request_id must contain 8-100 characters")
		return
	}
	if _, err := h.svc.ClaimReferralRewards(r.Context(), currentUserID(r), requestID); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	h.writeReferralSummary(w, r)
}

func (h *UserAPI) writeReferralSummary(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	summary, err := h.svc.GetReferralSummary(r.Context(), userID)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	rewards, _, err := h.svc.ListReferralRewards(r.Context(), userID, 100, 0)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	now := time.Now()
	out := ReferralSummary{
		InviteCode:      summary.InviteCode,
		InviteLink:      referralLink(r, summary.InviteCode),
		InvitedCount:    summary.InviteCount,
		FrozenAmount:    millisToUSD(summary.PendingAmount),
		ClaimableAmount: millisToUSD(summary.AvailableAmount),
		ClaimedAmount:   millisToUSD(summary.CreditedAmount),
		Rewards:         make([]ReferralReward, 0, len(rewards)),
	}
	for _, reward := range rewards {
		status := ReferralRewardStatus("frozen")
		switch reward.Status {
		case domain.ReferralRewardCredited:
			status = ReferralRewardStatus("claimed")
		case domain.ReferralRewardReversed:
			status = ReferralRewardStatus("reversed")
		case domain.ReferralRewardPending:
			if !reward.AvailableAt.After(now) {
				status = ReferralRewardStatus("claimable")
			}
		}
		out.Rewards = append(out.Rewards, ReferralReward{
			Id:           reward.ID,
			InviteeEmail: reward.InviteeEmail,
			SourceType:   ReferralRewardSourceType(reward.SourceType),
			BaseAmount:   millisToUSD(reward.BaseAmount),
			RewardAmount: millisToUSD(reward.RewardAmount),
			Status:       status,
			AvailableAt:  reward.AvailableAt,
			ClaimedAt:    reward.CreditedAt,
			CreatedAt:    reward.CreatedAt,
		})
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

func referralLink(r *http.Request, code string) string {
	scheme := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	if scheme != "http" && scheme != "https" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host + "/user/register?ref=" + url.QueryEscape(code)
}

func millisToUSD(value int64) float64 { return float64(value) / 1e5 }
