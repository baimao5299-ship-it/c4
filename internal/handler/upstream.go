// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"net/url"
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

// PostUpstreamsValidateAll runs a complete model-capability check for every
// saved upstream and returns one independent result per row. A failed relay is
// represented inside the 200 response so the remaining upstreams are still
// checked and the operator can act on the full inventory in one click.
func (h *AdminAPI) PostUpstreamsValidateAll(w http.ResponseWriter, r *http.Request) {
	result, err := h.svc.ValidateAllUpstreams(r.Context())
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUpstreamValidationSummary(result))
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
	// Anchor legacy connection edits to the row version observed by this
	// request. The service performs a second read before validating a changed
	// endpoint or credential; carrying the first read prevents a stale form that
	// raced with that read from silently adopting the newer configuration. A
	// name/multiplier-only legacy edit intentionally stays unversioned because
	// health telemetry can advance UpdatedAt independently.
	baseChanged := current != nil && canonicalUpstreamBaseURL(in.BaseUrl) != canonicalUpstreamBaseURL(current.BaseURL)
	keyChanged := false
	if current != nil {
		if in.UpstreamKey != nil && strings.TrimSpace(*in.UpstreamKey) != "" {
			// Compare the normalized copy/paste form so resubmitting the same
			// credential does not create a spurious revision conflict when
			// background telemetry has advanced UpdatedAt.
			keyChanged = canonicalUpstreamKey(*in.UpstreamKey) != canonicalUpstreamKey(deref(current.UpstreamKey))
		}
		if deref(in.ClearUpstreamKey) && canonicalUpstreamKey(deref(current.UpstreamKey)) != "" {
			keyChanged = true
		}
	}
	if u.ExpectedUpdatedAt == nil && current != nil && (baseChanged || keyChanged) {
		revision := current.UpdatedAt
		u.ExpectedUpdatedAt = &revision
	}
	// The service performs capability validation before committing endpoint or
	// credential changes, so a slow/changed upstream cannot leave an unroutable
	// row saved between the browser's preview and PUT.
	saved, err := h.svc.UpdateUpstreamWithModelValidation(r.Context(), u)
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

// canonicalUpstreamBaseURL mirrors the service's address comparison for the
// management-only revision decision. Older clients commonly submit a copied
// trailing /v1; that spelling is equivalent to the stored bare root and must
// not turn a non-connection edit into a versioned conflict.
func canonicalUpstreamBaseURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Path == "" {
		return value
	}
	path := strings.TrimRight(parsed.Path, "/")
	lower := strings.ToLower(path)
	if lower == "/v1" || strings.HasSuffix(lower, "/v1") {
		path = path[:len(path)-len("/v1")]
		if path == "/" {
			path = ""
		}
		parsed.Path = path
		parsed.RawPath = ""
		value = parsed.String()
	}
	return strings.TrimRight(value, "/")
}

// canonicalUpstreamKey mirrors the service's credential normalization for the
// management-only revision decision. Keys are write-only in API responses, so
// an operator may paste the same value with one or more Bearer prefixes.
func canonicalUpstreamKey(value string) string {
	value = strings.TrimSpace(value)
	for value != "" {
		fields := strings.Fields(value)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "bearer") {
			break
		}
		value = strings.TrimSpace(value[len(fields[0]):])
	}
	return value
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
		// Keep a verified empty catalogue as [] rather than turning it into JSON
		// null. The distinction is meaningful: [] means the last complete probe
		// found no routable models, while nil means no snapshot is known.
		copyModels := append([]string{}, u.Models...)
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

func toAPIUpstreamValidationSummary(result *service.UpstreamValidationSummary) UpstreamValidationSummary {
	if result == nil {
		return UpstreamValidationSummary{Items: []UpstreamValidationItem{}}
	}
	items := make([]UpstreamValidationItem, 0, len(result.Items))
	for _, item := range result.Items {
		models := append([]string{}, item.Models...)
		var code *UpstreamValidationItemErrorCode
		if item.ErrorCode != "" {
			v := UpstreamValidationItemErrorCode(item.ErrorCode)
			code = &v
		}
		items = append(items, UpstreamValidationItem{
			Upstream:           toAPIUpstream(item.Upstream),
			Attempted:          item.Attempted,
			Models:             models,
			ModelsTotal:        item.ModelsTotal,
			ModelsChecked:      item.ModelsChecked,
			ModelsAvailable:    item.ModelsAvailable,
			ModelsFailed:       item.ModelsFailed,
			ValidationComplete: item.ValidationComplete,
			Ok:                 item.OK,
			LatencyMs:          item.LatencyMS,
			ErrorCode:          code,
		})
	}
	return UpstreamValidationSummary{
		Total:      result.Total,
		Completed:  result.Completed,
		Passed:     result.Passed,
		Failed:     result.Failed,
		DurationMs: result.DurationMS,
		Items:      items,
	}
}
