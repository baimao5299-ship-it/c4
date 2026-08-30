// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// UpstreamStore is optional so existing integrations and lightweight test stores
// do not need a breaking interface change. The repository implements it; a
// deployment without the table receives a clear configuration error.
type UpstreamStore interface {
	CreateUpstream(context.Context, *domain.Upstream) (*domain.Upstream, error)
	GetUpstream(context.Context, int64) (*domain.Upstream, error)
	ListUpstreams(context.Context, repository.ListQuery) ([]*domain.Upstream, int64, error)
	UpdateUpstream(context.Context, *domain.Upstream) (*domain.Upstream, error)
	SetUpstreamEnabled(context.Context, int64, bool) (*domain.Upstream, error)
	DeleteUpstream(context.Context, int64) error
	RecordUpstreamProbe(context.Context, *domain.Upstream, bool, int64, *string) (*domain.Upstream, error)
	RecordUpstreamBalance(context.Context, *domain.Upstream, *string, *string, string, *time.Time) (*domain.Upstream, error)
}

// UpstreamModelStore is an optional capability persistence surface. Keeping it
// separate preserves source compatibility for lightweight integrations while
// production repositories retain the last verified /v1/models catalogue.
type UpstreamModelStore interface {
	RecordUpstreamModels(context.Context, *domain.Upstream, []string, *string) (*domain.Upstream, error)
}

// GroupUpstreamStore persists the per-group upstream relation. It is kept
// optional, just like UpstreamStore, so existing lightweight integrations that
// only use account groups remain source-compatible until the relation is used.
type GroupUpstreamStore interface {
	ListGroupUpstreams(context.Context, int64) ([]*domain.GroupUpstream, error)
	SetGroupUpstreams(context.Context, int64, []*domain.GroupUpstream) error
}

// GroupRoutingStore creates an upstream-routed group and its members in one
// database transaction. It is optional so existing integrations can keep the
// legacy two-step group API until their repository is upgraded.
type GroupRoutingStore interface {
	CreateGroupWithUpstreams(context.Context, *domain.Group, []*domain.GroupUpstream) (*domain.Group, error)
	UpdateGroupWithUpstreams(context.Context, *domain.Group, []*domain.GroupUpstream) (*domain.Group, error)
}

type UpstreamProbeResult struct {
	Upstream  *domain.Upstream
	OK        bool
	LatencyMS int64
	ErrorCode string
}

// UpstreamModelsResult is the bounded, non-secret result of reading and
// validating an upstream's model catalogue. The UI and scheduler consume only
// models that completed a real request successfully.
type UpstreamModelsResult struct {
	Models             []string
	OK                 bool
	ErrorCode          string
	ModelsTotal        int
	ModelsChecked      int
	ModelsAvailable    int
	ModelsFailed       int
	ValidationComplete bool
}

// UpstreamModelValidationError keeps the failed validation category visible at
// the management boundary while still mapping to the normal invalid-input HTTP
// response. No upstream row is created when validation does not complete with
// at least one usable model.
type UpstreamModelValidationError struct {
	Code string
}

func (e *UpstreamModelValidationError) Error() string {
	code := strings.TrimSpace(e.Code)
	if code == "" {
		code = "model_unavailable"
	}
	return fmt.Sprintf("%v: upstream model validation failed (%s)", ErrInvalidInput, code)
}

func (e *UpstreamModelValidationError) Unwrap() error { return ErrInvalidInput }

const (
	upstreamModelValidationConcurrency = 4
	upstreamModelValidationTimeout     = 30 * time.Second
	upstreamModelValidationPerModel    = 5 * time.Second
)

var errUpstreamStoreUnavailable = errors.New("upstream management is not configured")

const upstreamBalanceStaleFor = 15 * time.Minute

func (s *Service) upstreamStore() (UpstreamStore, error) {
	if s == nil || s.upstreams == nil {
		return nil, errUpstreamStoreUnavailable
	}
	return s.upstreams, nil
}

func (s *Service) ListUpstreams(ctx context.Context, q repository.ListQuery) ([]*domain.Upstream, int64, error) {
	store, err := s.upstreamStore()
	if err != nil {
		return nil, 0, err
	}
	if err := validateListQuery(q, listSortFields["upstreams"]); err != nil {
		return nil, 0, err
	}
	for _, status := range q.StatusList {
		if status != "active" && status != "disabled" {
			return nil, 0, ErrInvalidInput
		}
	}
	rows, total, err := store.ListUpstreams(ctx, q)
	if err != nil {
		return nil, 0, mapRepoErr(err)
	}
	return rows, total, nil
}

func (s *Service) GetUpstream(ctx context.Context, id int64) (*domain.Upstream, error) {
	store, err := s.upstreamStore()
	if err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrInvalidInput
	}
	u, err := store.GetUpstream(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return u, nil
}

func (s *Service) CreateUpstream(ctx context.Context, u *domain.Upstream) (*domain.Upstream, error) {
	store, err := s.upstreamStore()
	if err != nil {
		return nil, err
	}
	if u != nil && u.ClearUpstreamKey && u.UpstreamKey != nil {
		return nil, ErrInvalidInput
	}
	if err := validateUpstream(u); err != nil {
		return nil, err
	}
	created, err := store.CreateUpstream(ctx, u)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.invalidateUpstreamConfig(ctx)
	return created, nil
}

// CreateUpstreamWithModelValidation validates the endpoint/key and persists the
// verified model snapshot as one management operation. Keeping this server-side
// prevents a browser from doing a full paid catalogue probe before creation and
// then immediately repeating it after the row is saved.
func (s *Service) CreateUpstreamWithModelValidation(ctx context.Context, u *domain.Upstream) (*domain.Upstream, error) {
	store, err := s.upstreamStore()
	if err != nil {
		return nil, err
	}
	if _, ok := store.(UpstreamModelStore); !ok {
		return nil, fmt.Errorf("%w: model snapshot storage is not configured", ErrInvalidInput)
	}
	if u != nil && u.ClearUpstreamKey && u.UpstreamKey != nil {
		return nil, ErrInvalidInput
	}
	if err := validateUpstream(u); err != nil {
		return nil, err
	}
	base := strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
	key := strings.TrimSpace(derefUpstreamKey(u.UpstreamKey))
	client := s.managementHTTPClient()
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	result := validateUpstreamModels(ctx, client, base, key)
	if !result.ValidationComplete || !result.OK {
		return nil, &UpstreamModelValidationError{Code: result.ErrorCode}
	}
	// Carry the verified capability snapshot into the create mutation. The
	// repository writes these fields with the row, so a successful response can
	// never advertise a model catalogue that was not persisted.
	u.Models = append([]string(nil), result.Models...)
	checkedAt := time.Now()
	u.ModelsCheckedAt = &checkedAt
	u.ModelsError = optionalString(result.ErrorCode)
	created, err := store.CreateUpstream(ctx, u)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.invalidateUpstreamConfig(ctx)
	return created, nil
}

func (s *Service) UpdateUpstream(ctx context.Context, u *domain.Upstream) (*domain.Upstream, error) {
	store, err := s.upstreamStore()
	if err != nil {
		return nil, err
	}
	if u == nil || u.ID <= 0 {
		return nil, ErrInvalidInput
	}
	current, err := store.GetUpstream(ctx, u.ID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if current == nil || current.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d deleted", ErrNotFound, u.ID)
	}
	if u.ClearUpstreamKey && u.UpstreamKey != nil {
		return nil, ErrInvalidInput
	}
	// PUT is a full management form, but secrets and read-only health fields are
	// intentionally omitted by the browser. An unchanged endpoint can retain its
	// write-only key. A changed endpoint must receive a new key or an explicit
	// clear instruction so it cannot be replayed to an unrelated upstream.
	if u.ClearUpstreamKey {
		u.UpstreamKey = nil
	} else if u.UpstreamKey == nil {
		u.UpstreamKey = current.UpstreamKey
	}
	if u.Note == nil {
		u.Note = current.Note
	}
	if err := validateUpstream(u); err != nil {
		return nil, err
	}
	baseURLChanged := current.BaseURL != u.BaseURL
	keyChanged := upstreamKeysDiffer(current.UpstreamKey, u.UpstreamKey)
	balanceConfigChanged := upstreamBalanceConfigChanged(current, u)
	if baseURLChanged && hasUpstreamKey(current.UpstreamKey) && !u.ClearUpstreamKey && !keyChanged {
		return nil, fmt.Errorf("%w: a new upstream key is required when changing the address", ErrInvalidInput)
	}
	if baseURLChanged || keyChanged {
		u.ResetTelemetry = true
		u.BalanceAmount = nil
		u.BalanceCurrency = nil
		u.BalanceStatus = domain.UpstreamBalanceUnconfigured
		u.BalanceCheckedAt = nil
	} else if balanceConfigChanged {
		// A balance response belongs to its endpoint and JSON mapping. Do not
		// reuse it after either one changes, but preserve unrelated probe history.
		u.BalanceAmount = nil
		u.BalanceCurrency = nil
		u.BalanceStatus = domain.UpstreamBalanceUnconfigured
		u.BalanceCheckedAt = nil
	} else {
		u.BalanceAmount = current.BalanceAmount
		u.BalanceCurrency = current.BalanceCurrency
		u.BalanceStatus = current.BalanceStatus
		u.BalanceCheckedAt = current.BalanceCheckedAt
	}
	updated, err := store.UpdateUpstream(ctx, u)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.invalidateUpstreamConfig(ctx)
	return updated, nil
}

func (s *Service) DeleteUpstream(ctx context.Context, id int64) error {
	store, err := s.upstreamStore()
	if err != nil {
		return err
	}
	if id <= 0 {
		return ErrInvalidInput
	}
	if err := mapRepoErr(store.DeleteUpstream(ctx, id)); err != nil {
		return err
	}
	s.invalidateUpstreamConfig(ctx)
	return nil
}

func (s *Service) SetUpstreamEnabled(ctx context.Context, id int64, enabled bool) (*domain.Upstream, error) {
	store, err := s.upstreamStore()
	if err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrInvalidInput
	}
	updated, err := store.SetUpstreamEnabled(ctx, id, enabled)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.invalidateUpstreamConfig(ctx)
	return updated, nil
}

// ProbeUpstream performs a bounded direct request to the conventional
// OpenAI-compatible models endpoint. It records only a small error class, never
// response text or credentials, and does not alter account scheduling.
func (s *Service) ProbeUpstream(ctx context.Context, id int64) (*UpstreamProbeResult, error) {
	store, err := s.upstreamStore()
	if err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrInvalidInput
	}
	if ctx == nil {
		ctx = context.Background()
	}
	u, err := store.GetUpstream(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if u == nil || u.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d deleted", ErrNotFound, id)
	}
	base := strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := s.managementHTTPClient()
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	started := time.Now()
	// Use the same bounded /v1/models parser as model discovery. A reachable
	// portal or HTML error page may return HTTP 200; transport reachability alone
	// must not mark such an upstream healthy.
	_, code := fetchAdvertisedModels(probeCtx, client, base, strings.TrimSpace(derefUpstreamKey(u.UpstreamKey)))
	latency := time.Since(started).Milliseconds()
	ok := code == ""
	var errCode *string
	if code != "" {
		errCode = &code
	}
	updated, err := store.RecordUpstreamProbe(ctx, u, ok, latency, errCode)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return supersededUpstreamResult(ctx, store, id)
		}
		return nil, mapRepoErr(err)
	}
	return &UpstreamProbeResult{Upstream: updated, OK: ok, LatencyMS: latency, ErrorCode: code}, nil
}

// ListUpstreamModels reads the real model catalogue for a saved upstream. It
// returns HTTP-style failures as a bounded error code so the UI can explain the
// problem without exposing upstream response text or credentials.
func (s *Service) ListUpstreamModels(ctx context.Context, id int64) (*UpstreamModelsResult, error) {
	store, err := s.upstreamStore()
	if err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrInvalidInput
	}
	if ctx == nil {
		ctx = context.Background()
	}
	u, err := store.GetUpstream(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if u == nil || u.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d deleted", ErrNotFound, id)
	}
	base := strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}
	client := s.managementHTTPClient()
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	key := strings.TrimSpace(derefUpstreamKey(u.UpstreamKey))
	result := validateUpstreamModels(ctx, client, base, key)
	s.recordUpstreamModels(ctx, store, u, result.Models, result.ErrorCode)
	return &result, nil
}

func (s *Service) recordUpstreamModels(ctx context.Context, store UpstreamStore, expected *domain.Upstream, models []string, code string) {
	recorder, ok := store.(UpstreamModelStore)
	if !ok {
		return
	}
	var modelErr *string
	if code != "" {
		if code == "model_unavailable" {
			// Validation completed (possibly partially): persist only models that
			// answered a real request. Keeping the old list here could route
			// traffic to models that are no longer proven usable.
			modelErr = &code
		} else {
			modelErr = &code
			// Transport/auth failures happen before validation. Keep the last
			// verified catalogue while exposing the read error to operators.
			models = expected.Models
		}
	}
	if _, err := recorder.RecordUpstreamModels(ctx, expected, models, modelErr); err != nil {
		// A concurrent endpoint/key edit makes the probe stale. The read result is
		// still useful to the caller, while the next reload will use the new config.
		if s.log != nil && !errors.Is(err, repository.ErrConflict) {
			s.log.Warn("upstream model snapshot not saved", logx.Int64("id", expected.ID), logx.Error(err))
		}
		return
	}
	s.invalidateUpstreamConfig(ctx)
}

// PreviewUpstreamModels validates a not-yet-saved endpoint and key. It is used
// by the create form before persistence, so a typo or unsupported credential
// cannot create an unusable upstream record.
func (s *Service) PreviewUpstreamModels(ctx context.Context, base, key string) (*UpstreamModelsResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}
	client := s.managementHTTPClient()
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	result := validateUpstreamModels(ctx, client, base, strings.TrimSpace(key))
	return &result, nil
}

// TestUpstream keeps the existing programmatic API and uses the first real
// model for callers that do not need to choose one explicitly.
func (s *Service) TestUpstream(ctx context.Context, id int64) (*UpstreamProbeResult, error) {
	return s.TestUpstreamWithModel(ctx, id, "")
}

// TestUpstreamWithModel sends one tiny, non-streaming "hi" request through the
// same configured transport used by the gateway. The selected model must be in
// the current /v1/models catalogue. Responses-compatible relays are tried
// first; a format rejection falls back to Chat Completions.
func (s *Service) TestUpstreamWithModel(ctx context.Context, id int64, requestedModel string) (*UpstreamProbeResult, error) {
	store, err := s.upstreamStore()
	if err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrInvalidInput
	}
	if ctx == nil {
		ctx = context.Background()
	}
	u, err := store.GetUpstream(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if u == nil || u.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d deleted", ErrNotFound, id)
	}
	base := strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}
	testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := s.managementHTTPClient()
	client.Timeout = 12 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	key := strings.TrimSpace(derefUpstreamKey(u.UpstreamKey))
	models, modelCode := fetchAdvertisedModels(testCtx, client, base, key)
	model := strings.TrimSpace(requestedModel)
	if modelCode != "" {
		return s.recordUpstreamTestFailure(ctx, store, u, modelCode)
	}
	if model == "" {
		model = models[0]
	} else if !containsModel(models, model) {
		return s.recordUpstreamTestFailure(ctx, store, u, "model_unavailable")
	}
	started := time.Now()
	status, requestErr := sendUpstreamTestRequest(testCtx, client, base+"/v1/responses", key, model, false)
	if shouldFallbackTestRequest(status, requestErr) && testCtx.Err() == nil {
		status, requestErr = sendUpstreamTestRequest(testCtx, client, base+"/v1/chat/completions", key, model, true)
	}
	latency := time.Since(started).Milliseconds()
	ok := requestErr == nil && status >= 200 && status < 300
	code := classifyUpstreamTestError(testCtx, status, requestErr)
	var errCode *string
	if code != "" {
		errCode = &code
	}
	updated, err := store.RecordUpstreamProbe(ctx, u, ok, latency, errCode)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return supersededUpstreamResult(ctx, store, id)
		}
		return nil, mapRepoErr(err)
	}
	return &UpstreamProbeResult{Upstream: updated, OK: ok, LatencyMS: latency, ErrorCode: code}, nil
}

func (s *Service) recordUpstreamTestFailure(ctx context.Context, store UpstreamStore, u *domain.Upstream, code string) (*UpstreamProbeResult, error) {
	updated, err := store.RecordUpstreamProbe(ctx, u, false, 0, &code)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return supersededUpstreamResult(ctx, store, u.ID)
		}
		return nil, mapRepoErr(err)
	}
	return &UpstreamProbeResult{Upstream: updated, OK: false, ErrorCode: code}, nil
}

func fetchAdvertisedModels(ctx context.Context, client *http.Client, base, key string) ([]string, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	// OpenAI-compatible relays usually expose /v1/models, but a number of
	// gateways expose the same catalogue at /models when their configured base
	// URL already represents the versioned API root. Try that route only when
	// the canonical route explicitly looks missing; auth, network, rate-limit,
	// and provider failures must retain their original classification.
	paths := []string{"/v1/models", "/models"}
	lastCode := "http_error"
	for i, path := range paths {
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		models, code, status, retryable := fetchAdvertisedModelsPath(requestCtx, client, base+path, key)
		cancel()
		if code == "" {
			return models, ""
		}
		lastCode = code
		if i == len(paths)-1 || !retryable || status == 0 {
			return nil, code
		}
	}
	return nil, lastCode
}

// fetchAdvertisedModelsPath performs one bounded catalogue request. The
// boolean return identifies route-level failures for which the caller may try
// a compatibility path; it deliberately excludes model/auth/provider errors.
func fetchAdvertisedModelsPath(ctx context.Context, client *http.Client, target, key string) ([]string, string, int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "invalid_value", 0, false
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil || resp == nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, "timeout", 0, false
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, "canceled", 0, false
		}
		return nil, "network", 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, "auth", resp.StatusCode, false
		case http.StatusTooManyRequests:
			return nil, "rate_limited", resp.StatusCode, false
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return nil, "upstream", resp.StatusCode, false
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
			return nil, "http_error", resp.StatusCode, true
		default:
			return nil, "http_error", resp.StatusCode, false
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return nil, "network", resp.StatusCode, false
	}
	if len(body) > 1<<20 {
		return nil, "invalid_value", resp.StatusCode, false
	}
	models := parseUpstreamModels(body)
	if len(models) == 0 {
		return nil, "invalid_value", resp.StatusCode, false
	}
	return models, "", resp.StatusCode, false
}

type upstreamModelValidation struct {
	Models             []string
	OK                 bool
	ErrorCode          string
	ModelsTotal        int
	ModelsChecked      int
	ModelsAvailable    int
	ModelsFailed       int
	ValidationComplete bool
}

// validateUpstreamModels turns an advertised catalogue into a verified
// catalogue. Every model receives one tiny non-streaming request, bounded by a
// worker pool and per-model/overall deadlines. The route is Responses first;
// only an explicit protocol/format rejection is retried as Chat Completions.
func validateUpstreamModels(ctx context.Context, client *http.Client, base, key string) UpstreamModelsResult {
	models, code := fetchAdvertisedModels(ctx, client, base, key)
	if code != "" {
		return UpstreamModelsResult{Models: nil, OK: false, ErrorCode: code}
	}
	result := validateModelCatalogue(ctx, client, base, key, models)
	return UpstreamModelsResult{
		Models:             result.Models,
		OK:                 len(result.Models) > 0,
		ErrorCode:          result.ErrorCode,
		ModelsTotal:        result.ModelsTotal,
		ModelsChecked:      result.ModelsChecked,
		ModelsAvailable:    result.ModelsAvailable,
		ModelsFailed:       result.ModelsFailed,
		ValidationComplete: result.ValidationComplete,
	}
}

func validateModelCatalogue(ctx context.Context, client *http.Client, base, key string, models []string) upstreamModelValidation {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(models) == 0 {
		return upstreamModelValidation{ValidationComplete: true}
	}
	validationCtx, cancel := context.WithTimeout(ctx, upstreamModelValidationTimeout)
	defer cancel()
	type outcome struct {
		model   string
		ok      bool
		checked bool
		code    string
	}
	jobs := make(chan string)
	results := make(chan outcome, len(models))
	workers := upstreamModelValidationConcurrency
	if len(models) < workers {
		workers = len(models)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for model := range jobs {
				if validationCtx.Err() != nil {
					results <- outcome{model: model, checked: false}
					continue
				}
				// Only count models for which a worker actually started the
				// bounded request. Jobs skipped after the overall deadline are
				// not reported as real validation attempts.
				modelCtx, modelCancel := context.WithTimeout(validationCtx, upstreamModelValidationPerModel)
				status, requestErr := sendUpstreamTestRequest(modelCtx, client, base+"/v1/responses", key, model, false)
				if shouldFallbackTestRequest(status, requestErr) && modelCtx.Err() == nil {
					status, requestErr = sendUpstreamTestRequest(modelCtx, client, base+"/v1/chat/completions", key, model, true)
				}
				modelCancel()
				modelCode := classifyModelValidationError(validationCtx, status, requestErr)
				results <- outcome{model: model, ok: modelCode == "", checked: true, code: modelCode}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, model := range models {
			select {
			case <-validationCtx.Done():
				return
			case jobs <- model:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	valid := make([]string, 0, len(models))
	checked := 0
	failed := 0
	counts := make(map[string]int)
	for outcome := range results {
		if outcome.checked {
			checked++
		}
		if outcome.ok {
			valid = append(valid, outcome.model)
		} else if outcome.checked {
			failed++
			if outcome.code != "" {
				counts[outcome.code]++
			}
		}
	}
	// Preserve the provider's catalogue order. The UI applies its own stable
	// latest-first presentation ordering after this verification step.
	validSet := make(map[string]struct{}, len(valid))
	for _, model := range valid {
		validSet[model] = struct{}{}
	}
	ordered := make([]string, 0, len(valid))
	for _, model := range models {
		if _, ok := validSet[model]; ok {
			ordered = append(ordered, model)
		}
	}
	complete := checked == len(models) && validationCtx.Err() == nil
	validationCode := ""
	if !complete {
		// A partial run is not a capability snapshot. Even if some workers
		// succeeded, keep the previous verified catalogue and require a retry;
		// publishing a partial list would make the scheduler silently hide models
		// that were never checked.
		switch {
		case errors.Is(validationCtx.Err(), context.DeadlineExceeded):
			validationCode = "timeout"
		case errors.Is(validationCtx.Err(), context.Canceled):
			validationCode = "canceled"
		default:
			validationCode = dominantModelValidationError(counts)
		}
	} else if failed > 0 {
		// A complete run can safely publish the verified subset while explaining
		// that advertised models which failed the real request were filtered.
		validationCode = dominantModelValidationError(counts)
		if len(ordered) > 0 {
			validationCode = "model_unavailable"
		}
	}
	return upstreamModelValidation{
		Models:             ordered,
		OK:                 len(ordered) > 0,
		ErrorCode:          validationCode,
		ModelsTotal:        len(models),
		ModelsChecked:      checked,
		ModelsAvailable:    len(ordered),
		ModelsFailed:       failed,
		ValidationComplete: complete,
	}
}

func classifyModelValidationError(ctx context.Context, status int, requestErr error) string {
	if requestErr != nil {
		// A valid JSON error envelope means the model endpoint answered, but the
		// advertised model was rejected. Hide only that model and retain other
		// confirmed models.
		var envelope *upstreamErrorEnvelope
		if errors.As(requestErr, &envelope) {
			if isModelUnavailableMessage(string(envelope.body)) {
				return "model_unavailable"
			}
			return "invalid_response"
		}
		var httpErr *upstreamHTTPError
		if errors.As(requestErr, &httpErr) {
			if isModelUnavailableMessage(string(httpErr.body)) {
				return "model_unavailable"
			}
		}
	}
	code := classifyUpstreamTestError(ctx, status, requestErr)
	if code == "http_error" && (status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusUnprocessableEntity) {
		return "model_unavailable"
	}
	return code
}

func dominantModelValidationError(counts map[string]int) string {
	if len(counts) == 0 {
		return "model_unavailable"
	}
	// Prefer causes that require an upstream configuration or availability fix;
	// model_unavailable is the fallback when no stronger signal exists.
	priority := []string{"auth", "rate_limited", "timeout", "upstream", "network", "model_unavailable", "invalid_response", "http_error"}
	winner, best := "model_unavailable", 0
	for _, code := range priority {
		if count := counts[code]; count > best {
			winner, best = code, count
		}
	}
	return winner
}

func isModelUnavailableMessage(message string) bool {
	message = strings.ToLower(message)
	for _, marker := range []string{"model not found", "model_not_found", "unknown model", "invalid model", "model does not exist", "no such model", "model unavailable"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func parseUpstreamModels(body []byte) []string {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	var entries []any
	switch value := root.(type) {
	case []any:
		entries = value
	case map[string]any:
		for _, key := range []string{"data", "models"} {
			if candidate, ok := value[key].([]any); ok {
				entries = candidate
				break
			}
		}
	}
	seen := make(map[string]struct{}, len(entries))
	models := make([]string, 0, minInt(len(entries), 200))
	for _, entry := range entries {
		var name string
		switch value := entry.(type) {
		case string:
			name = value
		case map[string]any:
			for _, key := range []string{"id", "name", "model"} {
				if candidate, ok := value[key].(string); ok {
					name = candidate
					break
				}
			}
		}
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 200 {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		models = append(models, name)
		if len(models) == 200 {
			break
		}
	}
	return models
}

func containsModel(models []string, target string) bool {
	for _, model := range models {
		if model == target {
			return true
		}
	}
	return false
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func sendUpstreamTestRequest(ctx context.Context, client *http.Client, target, key, model string, chat bool) (int, error) {
	var body []byte
	if chat {
		body, _ = json.Marshal(map[string]any{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
			"stream":     false,
			"max_tokens": 1,
		})
	} else {
		body, _ = json.Marshal(map[string]any{
			"model":             model,
			"input":             "hi",
			"stream":            false,
			"store":             false,
			"max_output_tokens": 1,
		})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, errors.New("nil upstream response")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// A relay can return a portal/error page with HTTP 200. Treat that as a
		// failed test instead of reporting a false positive. Keep the body bound
		// so a malicious or misconfigured endpoint cannot exhaust the manager.
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
		if readErr != nil {
			return resp.StatusCode, readErr
		}
		if len(body) == 0 || len(body) > 1<<20 || !isJSONObjectResponse(body) {
			var envelope map[string]any
			if json.Unmarshal(body, &envelope) == nil {
				if _, hasError := envelope["error"]; hasError {
					return resp.StatusCode, &upstreamErrorEnvelope{body: append([]byte(nil), body...)}
				}
			}
			return resp.StatusCode, errInvalidUpstreamResponse
		}
		return resp.StatusCode, nil
	}
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, &upstreamHTTPError{status: resp.StatusCode, body: responseBody}
}

var errInvalidUpstreamResponse = errors.New("invalid upstream test response")
var errUpstreamErrorEnvelope = errors.New("upstream returned an error envelope")

type upstreamErrorEnvelope struct {
	body []byte
}

func (e *upstreamErrorEnvelope) Error() string { return errUpstreamErrorEnvelope.Error() }

func (e *upstreamErrorEnvelope) Unwrap() error { return errUpstreamErrorEnvelope }

type upstreamHTTPError struct {
	status int
	body   []byte
}

func (e *upstreamHTTPError) Error() string {
	if len(e.body) == 0 {
		return fmt.Sprintf("upstream returned HTTP %d", e.status)
	}
	return fmt.Sprintf("upstream returned HTTP %d: %s", e.status, strings.TrimSpace(string(e.body)))
}

func isJSONObjectResponse(body []byte) bool {
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return false
	}
	if value == nil {
		return false
	}
	// Some relays incorrectly return HTTP 200 for an application-level error.
	// Treating that envelope as a successful probe would mark a broken upstream
	// healthy and make the scheduler route real traffic into it. A top-level
	// error member is the common OpenAI-compatible shape; nested payload fields
	// remain provider-specific and are intentionally not rejected here.
	if _, hasError := value["error"]; hasError {
		return false
	}
	// A generic 200 JSON document (for example a portal's {"status":"ok"})
	// does not prove that the selected model endpoint answered. Require a
	// recognized response envelope used by the supported APIs instead of merely
	// accepting any arbitrary JSON object.
	if id, ok := value["id"].(string); ok && strings.TrimSpace(id) != "" {
		return true
	}
	if object, ok := value["object"].(string); ok && strings.TrimSpace(object) != "" {
		return true
	}
	if _, ok := value["choices"].([]any); ok {
		return true
	}
	if _, ok := value["output"].([]any); ok {
		return true
	}
	switch data := value["data"].(type) {
	case []any:
		return true
	case map[string]any:
		return len(data) > 0
	case string:
		return strings.TrimSpace(data) != ""
	}
	return false
}

func shouldFallbackTest(status int) bool {
	// A few OpenAI-compatible relays explicitly report that the newer
	// Responses API is not implemented (501) while still serving Chat
	// Completions. Treat it as a protocol capability rejection, alongside the
	// other format/path responses that already trigger the compatibility retry.
	return status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusUnsupportedMediaType || status == http.StatusUnprocessableEntity || status == http.StatusNotImplemented
}

// shouldFallbackTestRequest narrows the ambiguous 400/422 cases using the
// response text while keeping the legacy status-only helper for callers and
// tests that only have a status code. This prevents a model validation error
// from causing a second paid probe against the same relay.
func shouldFallbackTestRequest(status int, requestErr error) bool {
	if errors.Is(requestErr, errUpstreamErrorEnvelope) {
		var envelope *upstreamErrorEnvelope
		if errors.As(requestErr, &envelope) && isModelUnavailableMessage(string(envelope.body)) {
			return false
		}
		return true
	}
	if errors.Is(requestErr, errInvalidUpstreamResponse) {
		return true
	}
	if status == http.StatusNotFound {
		var httpErr *upstreamHTTPError
		if errors.As(requestErr, &httpErr) {
			message := strings.ToLower(string(httpErr.body))
			for _, marker := range []string{
				"model not found", "model_not_found", "unknown model", "invalid model",
				"model does not exist", "no such model", "model unavailable",
			} {
				if strings.Contains(message, marker) {
					return false
				}
			}
		}
		return true
	}
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return shouldFallbackTest(status)
	}
	var httpErr *upstreamHTTPError
	if !errors.As(requestErr, &httpErr) {
		return false
	}
	message := strings.ToLower(string(httpErr.body))
	for _, marker := range []string{
		"not implemented", "not supported", "unsupported", "not support",
		"method not allowed", "unknown endpoint", "unknown path", "content-type",
		"protocol", "responses api", "chat completions", "messages api",
		"invalid format", "format error", "route not found", "cannot decode",
		"failed to decode", "unmarshal",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func classifyUpstreamTestError(ctx context.Context, status int, requestErr error) string {
	if requestErr != nil {
		var httpErr *upstreamHTTPError
		if errors.As(requestErr, &httpErr) {
			requestErr = nil
		}
	}
	if requestErr != nil {
		if errors.Is(requestErr, errInvalidUpstreamResponse) || errors.Is(requestErr, errUpstreamErrorEnvelope) {
			return "invalid_response"
		}
		if errors.Is(requestErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "timeout"
		}
		if errors.Is(requestErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return "canceled"
		}
		return "network"
	}
	if status >= 200 && status < 300 {
		return ""
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "auth"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "upstream"
	default:
		return "http_error"
	}
}

// RefreshUpstreamBalance reads the optional generic JSON balance endpoint. It is
// always an explicit management action: a successful read replaces the snapshot;
// a failed read retains a known value as stale instead of fabricating a zero.
func (s *Service) RefreshUpstreamBalance(ctx context.Context, id int64) (*UpstreamProbeResult, error) {
	store, err := s.upstreamStore()
	if err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrInvalidInput
	}
	if ctx == nil {
		ctx = context.Background()
	}
	u, err := store.GetUpstream(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if u == nil || u.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d deleted", ErrNotFound, id)
	}
	client := s.managementHTTPClient()
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	account := billing.BalanceAccount{ID: id, BaseURL: u.BaseURL, UpstreamKey: derefUpstreamKey(u.UpstreamKey)}
	var result billing.ProviderBalance
	var fetchErr error
	if strings.TrimSpace(u.BalanceEndpoint) == "" {
		// Match CC Switch's provider-first balance lookup without making the
		// operator learn each relay's private dashboard path. The helper keeps a
		// bounded candidate list and uses this same proxy-aware client.
		result, fetchErr = billing.FetchAutoBalance(ctx, account, client)
	} else {
		auth := billing.HTTPAuthStyle(strings.ToLower(strings.TrimSpace(u.BalanceAuth)))
		if auth != billing.HTTPAuthNone && !hasUpstreamKey(u.UpstreamKey) {
			updated, err := recordFailedUpstreamBalance(ctx, store, u, "auth")
			if err != nil {
				if errors.Is(err, repository.ErrConflict) {
					return supersededUpstreamResult(ctx, store, id)
				}
				return nil, mapRepoErr(err)
			}
			return &UpstreamProbeResult{Upstream: updated, OK: false, ErrorCode: "auth"}, nil
		}
		adapter := billing.HTTPJSONAdapter{
			NameValue:       fmt.Sprintf("upstream-%d", id),
			Endpoint:        u.BalanceEndpoint,
			Method:          u.BalanceMethod,
			Auth:            auth,
			BalancePath:     u.BalancePath,
			CurrencyPath:    u.BalanceCurrencyPath,
			Timeout:         10 * time.Second,
			MaxResponseSize: 1 << 20,
			Client:          client,
		}
		if err := adapter.Validate(); err != nil {
			return nil, ErrInvalidInput
		}
		result, fetchErr = adapter.Fetch(ctx, account)
	}
	if fetchErr != nil || !validUpstreamBalanceAmount(result.Amount) {
		code := upstreamBalanceErrorCode(fetchErr)
		if fetchErr == nil {
			code = "invalid_value"
		}
		updated, err := recordFailedUpstreamBalance(ctx, store, u, code)
		if err != nil {
			if errors.Is(err, repository.ErrConflict) {
				return supersededUpstreamResult(ctx, store, id)
			}
			return nil, mapRepoErr(err)
		}
		return &UpstreamProbeResult{Upstream: updated, OK: false, ErrorCode: code}, nil
	}
	amount := strings.TrimSpace(result.Amount)
	currency := strings.TrimSpace(result.Currency)
	now := time.Now()
	updated, err := store.RecordUpstreamBalance(ctx, u, &amount, optionalString(currency), domain.UpstreamBalanceFresh, &now)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return supersededUpstreamResult(ctx, store, id)
		}
		return nil, mapRepoErr(err)
	}
	return &UpstreamProbeResult{Upstream: updated, OK: true}, nil
}

// managementHTTPClient returns a shallow copy so per-request timeout and
// redirect policy never mutate the shared proxy transport or its client.
func (s *Service) managementHTTPClient() *http.Client {
	if s != nil && s.upstreamHTTPClient != nil {
		copy := *s.upstreamHTTPClient
		return &copy
	}
	return &http.Client{Transport: &http.Transport{ForceAttemptHTTP2: true, DisableKeepAlives: true}}
}

func validateUpstream(u *domain.Upstream) error {
	if u == nil || strings.TrimSpace(u.Name) == "" || len(strings.TrimSpace(u.Name)) > 200 {
		return ErrInvalidInput
	}
	u.Name = strings.TrimSpace(u.Name)
	u.BaseURL = strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
	if err := validateBaseURL(u.BaseURL); err != nil {
		return err
	}
	if u.MultiplierBP < 0 || u.MultiplierBP > 1_000_000 {
		return ErrInvalidInput
	}
	if err := normalizeUpstreamBalanceConfig(u); err != nil {
		return err
	}
	if u.BalanceStatus == "" {
		u.BalanceStatus = domain.UpstreamBalanceUnconfigured
	}
	switch u.BalanceStatus {
	case domain.UpstreamBalanceFresh, domain.UpstreamBalanceStale,
		domain.UpstreamBalanceUnavailable, domain.UpstreamBalanceUnconfigured:
	default:
		return ErrInvalidInput
	}
	if u.UpstreamKey != nil {
		key := strings.TrimSpace(*u.UpstreamKey)
		if key == "" {
			u.UpstreamKey = nil
		} else {
			u.UpstreamKey = &key
		}
	}
	if u.Note != nil {
		note := strings.TrimSpace(*u.Note)
		if len(note) > 2000 {
			return ErrInvalidInput
		}
		u.Note = &note
	}
	return nil
}

func hasUpstreamKey(key *string) bool {
	return key != nil && strings.TrimSpace(*key) != ""
}

func upstreamKeysDiffer(left, right *string) bool {
	leftValue, rightValue := "", ""
	if left != nil {
		leftValue = strings.TrimSpace(*left)
	}
	if right != nil {
		rightValue = strings.TrimSpace(*right)
	}
	return leftValue != rightValue
}

func upstreamBalanceConfigChanged(current, next *domain.Upstream) bool {
	if current == nil || next == nil {
		return current != next
	}
	return strings.TrimSpace(current.BalanceEndpoint) != strings.TrimSpace(next.BalanceEndpoint) ||
		strings.EqualFold(strings.TrimSpace(current.BalanceMethod), strings.TrimSpace(next.BalanceMethod)) == false ||
		strings.EqualFold(strings.TrimSpace(current.BalanceAuth), strings.TrimSpace(next.BalanceAuth)) == false ||
		strings.TrimSpace(current.BalancePath) != strings.TrimSpace(next.BalancePath) ||
		strings.TrimSpace(current.BalanceCurrencyPath) != strings.TrimSpace(next.BalanceCurrencyPath)
}

func normalizeUpstreamBalanceConfig(u *domain.Upstream) error {
	u.BalanceEndpoint = strings.TrimSpace(u.BalanceEndpoint)
	u.BalanceMethod = strings.ToUpper(strings.TrimSpace(u.BalanceMethod))
	u.BalanceAuth = strings.ToLower(strings.TrimSpace(u.BalanceAuth))
	u.BalancePath = strings.TrimSpace(u.BalancePath)
	u.BalanceCurrencyPath = strings.TrimSpace(u.BalanceCurrencyPath)
	if u.BalanceEndpoint == "" {
		if u.BalancePath != "" || u.BalanceCurrencyPath != "" {
			return ErrInvalidInput
		}
		// UI defaults such as GET/none are harmless without an endpoint. Drop
		// them so an unconfigured balance reader has one canonical form.
		u.BalanceMethod = ""
		u.BalanceAuth = ""
		return nil
	}
	endpoint, err := url.Parse(u.BalanceEndpoint)
	if err != nil || endpoint.User != nil || endpoint.Fragment != "" || endpoint.RawQuery != "" {
		return ErrInvalidInput
	}
	if endpoint.IsAbs() {
		base, baseErr := url.Parse(u.BaseURL)
		if baseErr != nil || !isHTTPURL(endpoint) || !isHTTPURL(base) ||
			!strings.EqualFold(endpoint.Scheme, base.Scheme) || !strings.EqualFold(endpoint.Host, base.Host) {
			return ErrInvalidInput
		}
	} else if endpoint.Host != "" || !strings.HasPrefix(endpoint.Path, "/") {
		return ErrInvalidInput
	}
	if u.BalanceMethod == "" {
		u.BalanceMethod = http.MethodGet
	}
	if u.BalanceMethod != http.MethodGet && u.BalanceMethod != http.MethodPost {
		return ErrInvalidInput
	}
	if u.BalanceAuth == "" {
		u.BalanceAuth = string(billing.HTTPAuthBearer)
	}
	if u.BalanceAuth != string(billing.HTTPAuthNone) && u.BalanceAuth != string(billing.HTTPAuthBearer) && u.BalanceAuth != string(billing.HTTPAuthAPIKey) {
		return ErrInvalidInput
	}
	if u.BalancePath == "" || !validJSONPath(u.BalancePath) || !validJSONPath(u.BalanceCurrencyPath) {
		return ErrInvalidInput
	}
	return nil
}

func isHTTPURL(value *url.URL) bool {
	if value == nil || value.Host == "" {
		return false
	}
	return strings.EqualFold(value.Scheme, "http") || strings.EqualFold(value.Scheme, "https")
}

func validJSONPath(path string) bool {
	if len(path) > 500 {
		return false
	}
	if path == "" {
		return true
	}
	for _, segment := range strings.Split(path, ".") {
		if segment == "" || len(segment) > 120 || strings.ContainsAny(segment, "\t\r\n") {
			return false
		}
	}
	return true
}

func validUpstreamBalanceAmount(value string) bool {
	amount, ok := new(big.Rat).SetString(strings.TrimSpace(value))
	return ok && amount.Sign() >= 0
}

func derefUpstreamKey(key *string) string {
	if key == nil {
		return ""
	}
	return strings.TrimSpace(*key)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func upstreamBalanceErrorCode(err error) string {
	switch billing.ClassifyBalanceError(err) {
	case billing.BalanceErrorNoEndpoint:
		return "unconfigured"
	case billing.BalanceErrorAuth:
		return "auth"
	case billing.BalanceErrorRateLimited:
		return "rate_limited"
	case billing.BalanceErrorTimeout:
		return "timeout"
	case billing.BalanceErrorCanceled:
		return "canceled"
	case billing.BalanceErrorInvalidValue:
		return "invalid_value"
	default:
		return "upstream"
	}
}

func recordFailedUpstreamBalance(ctx context.Context, store UpstreamStore, current *domain.Upstream, code string) (*domain.Upstream, error) {
	if code == "unconfigured" {
		return store.RecordUpstreamBalance(ctx, current, nil, nil, domain.UpstreamBalanceUnconfigured, nil)
	}
	if current.BalanceAmount != nil && current.BalanceCheckedAt != nil &&
		(current.BalanceStatus == domain.UpstreamBalanceFresh || current.BalanceStatus == domain.UpstreamBalanceStale) &&
		validUpstreamBalanceAmount(*current.BalanceAmount) &&
		time.Since(*current.BalanceCheckedAt) <= upstreamBalanceStaleFor {
		return store.RecordUpstreamBalance(ctx, current, current.BalanceAmount, current.BalanceCurrency, domain.UpstreamBalanceStale, current.BalanceCheckedAt)
	}
	return store.RecordUpstreamBalance(ctx, current, nil, nil, domain.UpstreamBalanceUnavailable, nil)
}

func supersededUpstreamResult(ctx context.Context, store UpstreamStore, id int64) (*UpstreamProbeResult, error) {
	latest, err := store.GetUpstream(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return &UpstreamProbeResult{Upstream: latest, OK: false, ErrorCode: "superseded"}, nil
}
