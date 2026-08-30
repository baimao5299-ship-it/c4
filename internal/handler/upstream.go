// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

func (h *AdminAPI) GetUpstreams(w http.ResponseWriter, r *http.Request, params GetUpstreamsParams) {
	q := repository.ListQuery{
		Limit:  httpface.ClampLimit(deref(params.Limit)),
		Offset: deref(params.Offset),
		Name:   deref(params.Name),
		Sort:   string(deref(params.Sort)),
		Order:  string(deref(params.Order)),
	}
	if params.Status != nil {
		q.StatusList = []string{string(*params.Status)}
	}
	rows, total, err := h.svc.ListUpstreams(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	items := make([]Upstream, 0, len(rows))
	for _, row := range rows {
		items = append(items, toAPIUpstream(row))
	}
	httpface.WriteJSON(w, http.StatusOK, UpstreamListResponse{Total: total, Items: items})
}

func (h *AdminAPI) PostUpstreams(w http.ResponseWriter, r *http.Request) {
	var in UpstreamCreate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	u := upstreamFromBody(in, nil)
	// Creation performs one server-side catalogue read and real model
	// validation, then stores that verified snapshot with the new row. Keeping
	// the operation together avoids the browser probing twice (preview + save).
	saved, err := h.svc.CreateUpstreamWithModelValidation(r.Context(), u)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUpstream(saved))
}

func (h *AdminAPI) PostUpstreamsModels(w http.ResponseWriter, r *http.Request) {
	var in UpstreamModelsPreview
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	result, err := h.svc.PreviewUpstreamModels(r.Context(), in.BaseUrl, deref(in.UpstreamKey))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUpstreamModels(result))
}

func (h *AdminAPI) GetUpstreamsId(w http.ResponseWriter, r *http.Request, id int64) {
	u, err := h.svc.GetUpstream(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUpstream(u))
}

func (h *AdminAPI) PutUpstreamsId(w http.ResponseWriter, r *http.Request, id int64) {
	var in UpstreamCreate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	current, err := h.svc.GetUpstream(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	u := upstreamFromBody(in, current)
	u.ID = id
	saved, err := h.svc.UpdateUpstream(r.Context(), u)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUpstream(saved))
}

func (h *AdminAPI) DeleteUpstreamsId(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeleteUpstream(r.Context(), id); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

func (h *AdminAPI) PatchUpstreamsIdStatus(w http.ResponseWriter, r *http.Request, id int64) {
	var in UpstreamStatusUpdate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	updated, err := h.svc.SetUpstreamEnabled(r.Context(), id, in.Enabled)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUpstream(updated))
}

func (h *AdminAPI) PostUpstreamsIdProbe(w http.ResponseWriter, r *http.Request, id int64) {
	result, err := h.svc.ProbeUpstream(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUpstreamProbe(result))
}

func (h *AdminAPI) GetUpstreamsIdModels(w http.ResponseWriter, r *http.Request, id int64) {
	result, err := h.svc.ListUpstreamModels(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUpstreamModels(result))
}

func (h *AdminAPI) PostUpstreamsIdTest(w http.ResponseWriter, r *http.Request, id int64) {
	var in UpstreamTestBody
	if err := decodeOptional(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	result, err := h.svc.TestUpstreamWithModel(r.Context(), id, deref(in.Model))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUpstreamProbe(result))
}

func (h *AdminAPI) PostUpstreamsIdBalance(w http.ResponseWriter, r *http.Request, id int64) {
	result, err := h.svc.RefreshUpstreamBalance(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUpstreamProbe(result))
}

func upstreamFromBody(in UpstreamCreate, current *domain.Upstream) *domain.Upstream {
	enabled := true
	multiplierBP := 10000
	balanceEndpoint, balanceMethod, balanceAuth := "", "", ""
	balancePath, balanceCurrencyPath := "", ""
	if current != nil {
		enabled = current.Enabled
		multiplierBP = current.MultiplierBP
		balanceEndpoint = current.BalanceEndpoint
		balanceMethod = current.BalanceMethod
		balanceAuth = current.BalanceAuth
		balancePath = current.BalancePath
		balanceCurrencyPath = current.BalanceCurrencyPath
	}
	if in.Enabled != nil {
		enabled = *in.Enabled
	} else if in.Status != nil {
		enabled = *in.Status != UpstreamCreateStatusDisabled
	}
	if in.MultiplierBp != nil {
		multiplierBP = *in.MultiplierBp
	}
	if in.BalanceEndpoint != nil {
		balanceEndpoint = *in.BalanceEndpoint
	}
	if in.BalanceMethod != nil {
		balanceMethod = string(*in.BalanceMethod)
	}
	if in.BalanceAuth != nil {
		balanceAuth = string(*in.BalanceAuth)
	}
	if in.BalancePath != nil {
		balancePath = *in.BalancePath
	}
	if in.BalanceCurrencyPath != nil {
		balanceCurrencyPath = *in.BalanceCurrencyPath
	}
	u := &domain.Upstream{
		Name:                in.Name,
		BaseURL:             in.BaseUrl,
		ExpectedUpdatedAt:   in.ExpectedUpdatedAt,
		MultiplierBP:        multiplierBP,
		Enabled:             enabled,
		ClearUpstreamKey:    deref(in.ClearUpstreamKey),
		BalanceEndpoint:     balanceEndpoint,
		BalanceMethod:       balanceMethod,
		BalanceAuth:         balanceAuth,
		BalancePath:         balancePath,
		BalanceCurrencyPath: balanceCurrencyPath,
		BalanceStatus:       domain.UpstreamBalanceUnconfigured,
	}
	if current != nil {
		u.BalanceStatus = current.BalanceStatus
		u.BalanceAmount = current.BalanceAmount
		u.BalanceCurrency = current.BalanceCurrency
		u.BalanceCheckedAt = current.BalanceCheckedAt
	}
	if in.UpstreamKey != nil && strings.TrimSpace(*in.UpstreamKey) != "" {
		key := strings.TrimSpace(*in.UpstreamKey)
		u.UpstreamKey = &key
	}
	if in.Note != nil {
		note := strings.TrimSpace(*in.Note)
		u.Note = &note
	}
	return u
}

func toAPIUpstream(u *domain.Upstream) Upstream {
	if u == nil {
		return Upstream{}
	}
	score, rating, average := u.Stability()
	multiplier := float64(u.MultiplierBP) / 10000
	configured := u.UpstreamKey != nil && strings.TrimSpace(*u.UpstreamKey) != ""
	// An upstream with no explicit balance fields uses the automatic
	// provider/fallback detector. Keep the flag true so the dashboard exposes
	// the refresh action instead of asking the operator for private JSON paths.
	balanceConfigured := strings.TrimSpace(u.BaseURL) != "" &&
		(strings.TrimSpace(u.BalanceEndpoint) == "" || strings.TrimSpace(u.BalancePath) != "")
	balanceMethod := strings.TrimSpace(u.BalanceMethod)
	if balanceMethod == "" {
		balanceMethod = http.MethodGet
	}
	balanceAuth := strings.TrimSpace(u.BalanceAuth)
	if balanceAuth == "" {
		balanceAuth = "bearer"
	}
	balanceStatus := UpstreamBalanceStatus(normalizeAPIBalanceStatus(u.BalanceStatus))
	// An amount without a fresh/stale status is not a usable current balance.
	// Hide legacy values at the API boundary so an unconfigured or unavailable
	// adapter cannot make the dashboard look like it has a live balance.
	balanceAmount := u.BalanceAmount
	balanceCurrency := u.BalanceCurrency
	balanceCheckedAt := u.BalanceCheckedAt
	if balanceStatus != UpstreamBalanceStatusFresh && balanceStatus != UpstreamBalanceStatusStale {
		balanceAmount = nil
		balanceCurrency = nil
		balanceCheckedAt = nil
	}
	var models *[]string
	if u.Models != nil {
		copyModels := append([]string(nil), u.Models...)
		models = &copyModels
	}
	out := Upstream{
		ID:                   u.ID,
		Name:                 u.Name,
		BaseURL:              u.BaseURL,
		MultiplierBP:         u.MultiplierBP,
		Multiplier:           &multiplier,
		Enabled:              u.Enabled,
		Status:               UpstreamStatusActive,
		CredentialConfigured: &configured,
		Note:                 u.Note,
		BalanceEndpoint:      u.BalanceEndpoint,
		BalanceMethod:        UpstreamBalanceMethod(balanceMethod),
		BalanceAuth:          UpstreamBalanceAuth(balanceAuth),
		BalancePath:          u.BalancePath,
		BalanceCurrencyPath:  u.BalanceCurrencyPath,
		BalanceConfigured:    balanceConfigured,
		BalanceAmount:        balanceAmount,
		BalanceCurrency:      balanceCurrency,
		BalanceStatus:        balanceStatus,
		BalanceCheckedAt:     balanceCheckedAt,
		RequestCount:         u.RequestCount,
		SuccessCount:         u.SuccessCount,
		FailureCount:         u.FailureCount,
		LatencyTotalMS:       u.LatencyTotalMS,
		LatencyMaxMS:         u.LatencyMaxMS,
		StabilityRating:      ptr(UpstreamStabilityRating(rating)),
		LastCheckedAt:        u.LastCheckedAt,
		LastSuccessAt:        u.LastSuccessAt,
		LastFailureAt:        u.LastFailureAt,
		LastError:            u.LastError,
		Models:               models,
		ModelsCheckedAt:      u.ModelsCheckedAt,
		ModelsError:          u.ModelsError,
		CreatedAt:            ptr(u.CreatedAt),
		UpdatedAt:            ptr(u.UpdatedAt),
	}
	if !u.Enabled {
		out.Status = UpstreamStatusDisabled
	}
	if u.RequestCount > 0 {
		out.AverageLatencyMS = &average
		out.StabilityScore = &score
	}
	return out
}

func normalizeAPIBalanceStatus(status string) string {
	switch status {
	case domain.UpstreamBalanceFresh, domain.UpstreamBalanceStale, domain.UpstreamBalanceUnavailable:
		return status
	default:
		return domain.UpstreamBalanceUnconfigured
	}
}

func toAPIUpstreamProbe(result *service.UpstreamProbeResult) UpstreamProbeResponse {
	if result == nil {
		return UpstreamProbeResponse{}
	}
	response := UpstreamProbeResponse{Ok: result.OK, Upstream: toAPIUpstream(result.Upstream), LatencyMs: result.LatencyMS}
	if result.ErrorCode != "" {
		code := UpstreamProbeResponseErrorCode(result.ErrorCode)
		response.ErrorCode = &code
	}
	return response
}

func toAPIUpstreamModels(result *service.UpstreamModelsResult) UpstreamModelsResponse {
	if result == nil {
		return UpstreamModelsResponse{Models: []string{}}
	}
	response := UpstreamModelsResponse{
		Ok:                 result.OK,
		Models:             result.Models,
		ModelsTotal:        result.ModelsTotal,
		ModelsChecked:      result.ModelsChecked,
		ModelsAvailable:    result.ModelsAvailable,
		ModelsFailed:       result.ModelsFailed,
		ValidationComplete: result.ValidationComplete,
	}
	if response.Models == nil {
		response.Models = []string{}
	}
	if result.ErrorCode != "" {
		code := UpstreamModelsResponseErrorCode(result.ErrorCode)
		response.ErrorCode = &code
	}
	return response
}
