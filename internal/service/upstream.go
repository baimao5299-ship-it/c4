// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

// UpstreamSnapshotStore is an optional read surface for long-running admin
// validation. Production repositories implement it with one ordered query so
// rows cannot move between OFFSET pages while probes are running. Lightweight
// integrations can keep implementing UpstreamStore only; the service retains a
// bounded, duplicate-safe fallback for them.
type UpstreamSnapshotStore interface {
	ListAllUpstreams(context.Context) ([]*domain.Upstream, error)
}

// UpstreamValidationLocker is implemented by the production repository when a
// shared database is available. It serializes long-running capability probes
// across application instances; lightweight stores may omit it and retain the
// in-process guard below.
type UpstreamValidationLocker interface {
	AcquireUpstreamValidationLock(context.Context) (release func(), ok bool, err error)
}

// UpstreamModelStore is an optional capability persistence surface. Keeping it
// separate preserves source compatibility for lightweight integrations while
// production repositories retain the last verified /v1/models catalogue.
type UpstreamModelStore interface {
	RecordUpstreamModels(context.Context, *domain.Upstream, []string, *string) (*domain.Upstream, error)
}

// UpstreamModelCapabilityStore atomically persists the verified model list and
// the concrete wire protocol that answered for each model. Production uses
// this richer surface; UpstreamModelStore remains as a compatibility fallback
// for lightweight integrations.
type UpstreamModelCapabilityStore interface {
	RecordUpstreamModelCapabilities(context.Context, *domain.Upstream, []string, map[string][]domain.RequestFormat, *string) (*domain.Upstream, error)
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

// upstreamAuthMode identifies the two credential headers used by the relay
// families supported by the gateway.  It intentionally stays private: the
// management API stores one opaque key and negotiates the wire header per
// request instead of making operators maintain another setting.
type upstreamAuthMode uint8

const (
	upstreamAuthBearer upstreamAuthMode = iota
	upstreamAuthAPIKey
)

// upstreamAuthModes returns a deterministic, bounded preference list.  Keys
// that look like Anthropic credentials start with x-api-key; all other keys
// retain the historical Bearer-first behavior.  The alternate header is only
// attempted after an authentication response, never after a model/provider
// failure, so a normal probe cannot be duplicated accidentally.
func upstreamAuthModes(key string, preferred upstreamAuthMode) []upstreamAuthMode {
	key = strings.ToLower(strings.TrimSpace(key))
	if strings.HasPrefix(key, "sk-ant-") || strings.Contains(key, "anthropic") {
		preferred = upstreamAuthAPIKey
	}
	if preferred == upstreamAuthAPIKey {
		return []upstreamAuthMode{upstreamAuthAPIKey, upstreamAuthBearer}
	}
	return []upstreamAuthMode{upstreamAuthBearer, upstreamAuthAPIKey}
}

func setUpstreamAuthHeader(req *http.Request, key string, mode upstreamAuthMode) {
	if req == nil {
		return
	}
	key = normalizeUpstreamKey(key)
	// Never leave a credential selected by a previous redirect/request on a
	// reused request object.  The redirect policy separately prevents sending
	// either header to another origin.
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	if key == "" {
		return
	}
	if mode == upstreamAuthAPIKey {
		req.Header.Set("x-api-key", key)
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
}

// shouldRetryUpstreamAuth is deliberately narrower than a generic 4xx retry.
// A model entitlement error must stay attached to that model. Empty or generic
// 401/403 responses are definitive; only a directional header/scheme complaint
// can justify trying the other conventional spelling.
func shouldRetryUpstreamAuth(status int, requestErr error) bool {
	return shouldRetryUpstreamAuthMode(status, requestErr, upstreamAuthBearer)
}

func shouldRetryUpstreamAuthMode(status int, requestErr error, mode upstreamAuthMode) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	body := strings.ToLower(strings.TrimSpace(responseErrorBody(requestErr)))
	// A missing body does not identify which header the relay expects. Retrying
	// blindly here doubles every model probe on an ordinary 401/403 and can
	// turn a fatal credential error into a false success when the second header
	// is ignored by a permissive router.
	if body == "" || isModelUnavailableMessage(body) {
		return false
	}
	// Only an explicit header/scheme instruction justifies trying the other
	// conventional credential spelling. Generic "invalid api key", "invalid
	// token", "unauthorized", and "forbidden" responses are definitive and
	// must not trigger another request.
	return isExplicitAuthHeaderMismatchForMode(body, mode)
}

func isExplicitAuthHeaderMismatch(message string) bool {
	return isExplicitAuthHeaderMismatchForMode(message, upstreamAuthBearer) ||
		isExplicitAuthHeaderMismatchForMode(message, upstreamAuthAPIKey)
}

func isExplicitAuthHeaderMismatchForMode(message string, mode upstreamAuthMode) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}
	// Keep this list directional: merely mentioning an auth header in a generic
	// error is not enough. The relay must say that the alternate spelling is
	// required, missing, expected, or preferred.
	markers := []string{}
	if mode == upstreamAuthAPIKey {
		markers = []string{
			"use authorization", "use the authorization", "use bearer", "use the bearer",
			"send authorization", "send the authorization", "send bearer", "send the bearer",
			"provide authorization", "provide the authorization", "provide bearer",
			"provide the bearer", "expected authorization", "expected bearer",
			"requires authorization", "require authorization", "requires bearer",
			"require bearer", "authorization is required", "bearer is required",
			"authorization header required", "bearer token required", "missing authorization",
			"missing bearer", "authorization instead", "bearer instead",
		}
	} else {
		markers = []string{
			"use x-api-key", "use the x-api-key", "send x-api-key", "send the x-api-key",
			"provide x-api-key", "provide the x-api-key", "expected x-api-key",
			"requires x-api-key", "require x-api-key", "x-api-key is required",
			"x-api-key header required", "missing x-api-key", "x-api-key instead",
		}
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// Scheme errors that name the expected scheme are directional even when the
	// relay omits the words "use" or "required".
	if strings.Contains(message, "authentication scheme") || strings.Contains(message, "auth scheme") {
		if mode == upstreamAuthAPIKey {
			return strings.Contains(message, "bearer") || strings.Contains(message, "authorization")
		}
		return strings.Contains(message, "x-api-key")
	}
	return false
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
	// advertisedModels and transientModels stay inside the service boundary.
	// They let snapshot retention distinguish a temporarily failed model from a
	// model that the provider definitively rejected or removed.
	advertisedModels []string
	transientModels  []string
	modelFormats     map[string][]domain.RequestFormat
}

// UpstreamValidationItem is the result of one complete capability check. The
// model slice is the exact subset that answered a real request successfully;
// it is never an advertised-only catalogue.
type UpstreamValidationItem struct {
	Upstream           *domain.Upstream
	Models             []string
	ModelsTotal        int
	ModelsChecked      int
	ModelsAvailable    int
	ModelsFailed       int
	ValidationComplete bool
	OK                 bool
	LatencyMS          int64
	ErrorCode          string
	// Attempted distinguishes a row that was actually handed to a worker from
	// rows left untouched after caller cancellation.
	Attempted bool
}

// UpstreamValidationSummary is returned by the one-click management action.
// Items are ordered by the upstream list order and include disabled rows so an
// operator can see exactly which records were checked.
type UpstreamValidationSummary struct {
	Total int
	// Completed counts only attempted rows whose complete model catalogue
	// validation finished. Rows that timed out, were canceled, or otherwise
	// produced an incomplete capability result remain in Items but are excluded.
	Completed  int
	Passed     int
	Failed     int
	DurationMS int64
	Items      []UpstreamValidationItem
}

// UpstreamValidationProgress is emitted after the snapshot is loaded and
// whenever one upstream finishes.  It deliberately reports counters only;
// credentials and response bodies never leave the service boundary.
type UpstreamValidationProgress struct {
	UpstreamsTotal   int
	UpstreamsChecked int
	ModelsTotal      int
	ModelsChecked    int
	ModelsAvailable  int
	ModelsFailed     int
	Done             bool
}

// UpstreamValidationProgressFunc receives a point-in-time progress snapshot.
// Callbacks must be short and non-blocking because they run on validation
// workers.
type UpstreamValidationProgressFunc func(UpstreamValidationProgress)

// UpstreamModelValidationError keeps the failed validation category visible at
// the management boundary while still mapping to the normal invalid-input HTTP
// response. A management write is rejected only when no model completed a real
// request; a partial usable subset is accepted with its transient error marker.
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
	// Model probes are deliberately serialized within one upstream. A number
	// of relays enforce a per-credential concurrency of one; probing four
	// models at once made the same credential return transient 429/timeout
	// responses even though an individual manual request succeeded.
	upstreamModelValidationConcurrency = 1
	// Keep the per-model budget aligned with the manual "hi" test (15s context,
	// 12s client timeout). The old 5s budget was a different contract and was
	// the main source of batch-only false negatives for slow relays.
	upstreamModelValidationTimeout  = 30 * time.Second
	upstreamModelValidationPerModel = 12 * time.Second
	upstreamValidationConcurrency   = 2
	upstreamValidationBatchLimit    = 200
	// Keep catalogue reads bounded independently from per-model probes. A
	// cursor-capable relay may expose many pages, but one broken page must not
	// hold the management lock for the full validation budget.
	upstreamModelCatalogueTimeout = 60 * time.Second
	// Bound the synchronous admin action as a whole. Per-upstream deadlines are
	// not sufficient when an inventory contains many slow or unreachable
	// relays: without this cap the HTTP request and the shared validation lock
	// could remain occupied for hours.
	// Serial model probes deliberately protect credentials that reject bursts.
	// The old five-minute cap only covered roughly 25 twelve-second probes and
	// therefore never reached the tail of a normal large catalogue. The async
	// management endpoint can expose this longer bounded run without holding an
	// HTTP request open.
	upstreamValidationBatchTimeout = 15 * time.Minute
	// Persist the outcome of a canceled validation for a short bounded window.
	// Client disconnects should not turn a diagnostic write into a misleading
	// storage failure, but a broken database must not keep the worker alive.
	upstreamValidationPersistenceTimeout = 5 * time.Second
	// Compatibility stores expose only ListUpstreams and may report a very
	// large (or unstable) total. Keep the fallback snapshot bounded so an
	// accidental count cannot turn one admin action into an unbounded read.
	upstreamValidationSnapshotMax     = 5000
	upstreamValidationSnapshotTimeout = 10 * time.Second
	// Keep model validation bounded while allowing substantially larger
	// provider catalogues than the legacy 200-entry limit. Oversized catalogues
	// are rejected as incomplete instead of being silently truncated.
	upstreamModelCatalogueMax = 5000
	// A provider catalogue may be cursor-paginated. Keep the number of GETs
	// bounded so a broken cursor cannot hold the validation lock indefinitely.
	upstreamModelCatalogueMaxPages = 100
	// Model catalogue reads are GETs and do not execute a model request. One
	// short retry removes a transient 429/503/network blip from the result
	// without turning discovery into an unbounded poll loop.
	upstreamModelCatalogueRetryDelay = 200 * time.Millisecond
	// Keep a single model's transient retry short. The model context remains
	// the authoritative budget, so an already-expired timeout is not replayed.
	upstreamModelProbeRetryDelay = 150 * time.Millisecond
)

// Keep the management "test one model" request bounded independently for each
// phase. The implicit-model path first reads /models and then sends a
// completion probe; sharing one deadline lets a slow catalogue consume the
// entire budget and falsely report a usable model as timed out. This is a
// variable (rather than a const) so package tests can shorten the bounded
// window without waiting 15 seconds to exercise the deadline boundary.
var upstreamManualModelTestTimeout = 15 * time.Second

// Some relays ignore stream=false and keep an SSE connection open after a
// delta frame. Give a subsequent failure frame a short opportunity to arrive
// before accepting that early frame as proof of capability; the bound keeps
// open keep-alive streams from turning a probe into a full timeout.
const upstreamProbeSSESettlementWindow = 200 * time.Millisecond

var errUpstreamStoreUnavailable = errors.New("upstream management is not configured")

const upstreamBalanceStaleFor = 15 * time.Minute

func (s *Service) upstreamStore() (UpstreamStore, error) {
	if s == nil || s.upstreams == nil {
		return nil, errUpstreamStoreUnavailable
	}
	return s.upstreams, nil
}

// lockUpstreamValidation serializes every management operation that probes
// model capabilities. The local mutex avoids duplicate work in one process;
// production repositories additionally take a session-level database lock so
// two application instances cannot probe and publish conflicting snapshots.
func (s *Service) lockUpstreamValidation(ctx context.Context) (func(), error) {
	if s == nil {
		return nil, errUpstreamStoreUnavailable
	}
	if !s.upstreamValidationMu.TryLock() {
		// Lock conflicts are service-level concurrency outcomes. Returning the
		// repository sentinel here bypasses the HTTP error mapper and turns a
		// normal "validation already running" condition into a misleading 500.
		return nil, fmt.Errorf("%w: another upstream model validation is in progress", ErrConflict)
	}
	releaseLocal := func() { s.upstreamValidationMu.Unlock() }
	if ctx == nil {
		ctx = context.Background()
	}
	// A request that is already canceled must retain the caller-facing
	// cancellation semantics (ValidateAll reports untouched rows explicitly).
	// Do not spend a pool connection trying to acquire the shared lock in that
	// case; no probe can legitimately start under the canceled context.
	if ctx.Err() != nil {
		return releaseLocal, nil
	}
	locker, ok := s.upstreams.(UpstreamValidationLocker)
	if !ok {
		return releaseLocal, nil
	}
	releaseRemote, acquired, err := locker.AcquireUpstreamValidationLock(ctx)
	if err != nil {
		if errors.Is(err, repository.ErrUpstreamValidationLockUnavailable) {
			// repository.New is retained for local/tool integrations without a
			// pgx pool. Keep those callers functional with the process-local guard;
			// production NewWithPG supplies the database-backed lock.
			return releaseLocal, nil
		}
		releaseLocal()
		return nil, mapRepoErr(err)
	}
	if !acquired || releaseRemote == nil {
		releaseLocal()
		if !acquired {
			return nil, fmt.Errorf("%w: another upstream model validation is in progress", ErrConflict)
		}
		return nil, fmt.Errorf("%w: upstream validation lock returned no release handle", ErrConflict)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseRemote()
			releaseLocal()
		})
	}, nil
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
	if s == nil {
		return nil, errUpstreamStoreUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	releaseValidation, err := s.lockUpstreamValidation(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseValidation()
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
	base := normalizeUpstreamBaseURL(u.BaseURL)
	key := normalizeUpstreamKey(derefUpstreamKey(u.UpstreamKey))
	client := s.managementHTTPClient()
	client.Timeout = upstreamModelValidationPerModel + time.Second
	client.CheckRedirect = upstreamCheckRedirect
	result := validateUpstreamModels(ctx, client, base, key)
	// A partial run is still useful when at least one model completed a real
	// request. Persist that confirmed subset and surface the transient error so
	// routing can continue for known-good models while an operator retries the
	// remaining catalogue later.
	if !result.OK {
		return nil, &UpstreamModelValidationError{Code: result.ErrorCode}
	}
	// Carry the verified capability snapshot into the create mutation. The
	// repository writes these fields with the row, so a successful response can
	// never advertise a model catalogue that was not persisted.
	// Use a non-nil empty slice for a verified empty catalogue so the JSON field
	// is persisted as [] (not NULL/unknown) and the scheduler excludes it.
	u.Models = append([]string{}, result.Models...)
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
	return s.updateUpstream(ctx, u, false)
}

// UpdateUpstreamWithModelValidation updates an upstream whose endpoint or
// credential changed only after the new connection has passed a real
// capability check. The check and conditional write live on the server so a
// browser cannot validate one configuration and then accidentally save another
// one after a concurrent edit. Name/multiplier-only edits keep the existing
// verified model snapshot and do not issue a paid probe.
func (s *Service) UpdateUpstreamWithModelValidation(ctx context.Context, u *domain.Upstream) (*domain.Upstream, error) {
	if s == nil {
		return nil, errUpstreamStoreUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	releaseValidation, err := s.lockUpstreamValidation(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseValidation()
	return s.updateUpstream(ctx, u, true)
}

// updateUpstream contains the shared management-form semantics. When
// validateModels is enabled, endpoint/key changes are capability-checked before
// persistence and the current revision is required even if an older client did
// not send expected_updated_at. Ordinary metadata/billing-form edits do not
// probe the upstream; legacy callers may omit the revision for those edits,
// while an explicitly supplied revision is still honored.
func (s *Service) updateUpstream(ctx context.Context, u *domain.Upstream, validateModels bool) (*domain.Upstream, error) {
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
	// Older rows may have been saved with a copied `/v1` suffix while the
	// service now stores canonical bare roots. Compare canonical values so an
	// equivalent edit does not demand a new key or wipe telemetry merely because
	// the spelling changed.
	currentBaseURL := normalizeUpstreamBaseURL(current.BaseURL)
	baseURLChanged := currentBaseURL != u.BaseURL
	keyChanged := upstreamKeysDiffer(current.UpstreamKey, u.UpstreamKey)
	balanceConfigChanged := upstreamBalanceConfigChanged(current, u)
	if baseURLChanged && hasUpstreamKey(current.UpstreamKey) && !u.ClearUpstreamKey && !keyChanged {
		return nil, fmt.Errorf("%w: a new upstream key is required when changing the address", ErrInvalidInput)
	}
	connectionChanged := baseURLChanged || keyChanged
	if validateModels && u.ExpectedUpdatedAt != nil && !current.UpdatedAt.Equal(*u.ExpectedUpdatedAt) {
		// An explicitly supplied revision is an optimistic-concurrency contract
		// for every management edit. Legacy callers may omit it, but a caller that
		// sends one must never overwrite a row changed by another window.
		return nil, fmt.Errorf("%w: id=%d changed", ErrConflict, u.ID)
	}
	if validateModels && connectionChanged {
		// Reject a stale form before any external request. Capability probes can
		// be billable and must never run for a revision that is already obsolete.
		// Legacy management clients may omit the revision field. Upgrade them
		// to the same optimistic condition used by the current UI. This guard is
		// only needed for a connection change because that is the operation that
		// performs an external capability probe.
		if u.ExpectedUpdatedAt == nil {
			revision := current.UpdatedAt
			u.ExpectedUpdatedAt = &revision
		}
	}
	if connectionChanged {
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
	if validateModels && connectionChanged {
		base := normalizeUpstreamBaseURL(u.BaseURL)
		key := normalizeUpstreamKey(derefUpstreamKey(u.UpstreamKey))
		client := s.managementHTTPClient()
		client.Timeout = upstreamModelValidationPerModel + time.Second
		client.CheckRedirect = upstreamCheckRedirect
		result := validateUpstreamModels(ctx, client, base, key)
		if !result.OK {
			return nil, &UpstreamModelValidationError{Code: result.ErrorCode}
		}
		u.Models = append([]string{}, result.Models...)
		checkedAt := time.Now()
		u.ModelsCheckedAt = &checkedAt
		u.ModelsError = optionalString(result.ErrorCode)
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
	releaseValidation, err := s.lockUpstreamValidation(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseValidation()
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
	base := normalizeUpstreamBaseURL(u.BaseURL)
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := s.managementHTTPClient()
	client.Timeout = 10 * time.Second
	client.CheckRedirect = upstreamCheckRedirect
	started := time.Now()
	// Use the same bounded /v1/models parser as model discovery. A reachable
	// portal or HTML error page may return HTTP 200; transport reachability alone
	// must not mark such an upstream healthy.
	_, code := fetchAdvertisedModels(probeCtx, client, base, normalizeUpstreamKey(derefUpstreamKey(u.UpstreamKey)))
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

func updatedOrFallbackUpstream(updated, fallback *domain.Upstream) *domain.Upstream {
	if updated != nil {
		return updated
	}
	return fallback
}

// recordExplicitUpstreamModel adds a model confirmed by a real operator test
// to the persisted capability snapshot. The model store is optional for
// lightweight integrations, so those callers keep the existing health-only
// behavior. A successful explicit probe is stronger evidence than a previous
// catalogue/transport warning: keeping that warning in ModelsError made the
// whole upstream look broken even though the selected model was routable.
func (s *Service) recordExplicitUpstreamModel(ctx context.Context, store UpstreamStore, expected *domain.Upstream, model string, format domain.RequestFormat) (*domain.Upstream, error) {
	recorder, ok := store.(UpstreamModelStore)
	if !ok || expected == nil {
		return expected, nil
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return expected, nil
	}
	models := mergeUpstreamModelLists(expected.Models, []string{model})
	modelFormats := cloneModelFormatSnapshot(expected.ModelFormats)
	if format.Valid() {
		modelFormats[model] = []domain.RequestFormat{format}
	}
	// ModelsError is a single upstream-level field, not a per-model result. A
	// successful explicit model request therefore clears it; any later batch
	// validation can repopulate it with a fresh warning for the current run.
	var saved *domain.Upstream
	var err error
	if capabilityRecorder, capabilityOK := store.(UpstreamModelCapabilityStore); capabilityOK {
		saved, err = capabilityRecorder.RecordUpstreamModelCapabilities(ctx, expected, models, modelFormats, nil)
	} else {
		saved, err = recorder.RecordUpstreamModels(ctx, expected, models, nil)
	}
	if err != nil {
		return nil, err
	}
	s.invalidateUpstreamConfig(ctx)
	return saved, nil
}

// ListUpstreamModels reads the real model catalogue for a saved upstream. It
// returns HTTP-style failures as a bounded error code so the UI can explain the
// problem without exposing upstream response text or credentials.
func (s *Service) ListUpstreamModels(ctx context.Context, id int64) (*UpstreamModelsResult, error) {
	if s == nil {
		return nil, errUpstreamStoreUnavailable
	}
	releaseValidation, err := s.lockUpstreamValidation(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseValidation()
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
	base := normalizeUpstreamBaseURL(u.BaseURL)
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}
	client := s.managementHTTPClient()
	client.Timeout = upstreamModelValidationPerModel + time.Second
	client.CheckRedirect = upstreamCheckRedirect
	key := normalizeUpstreamKey(derefUpstreamKey(u.UpstreamKey))
	result := validateUpstreamModels(ctx, client, base, key)
	// A transient catalogue/probe interruption must not hide models that were
	// already verified for this unchanged endpoint and credential. Keep the old
	// snapshot alongside any models confirmed by the current run; the warning
	// code still tells the operator that a retry is warranted.
	result.Models = retainedUpstreamModels(u, result.Models, result.advertisedModels, result.transientModels, result.ValidationComplete, result.ErrorCode)
	result.modelFormats = retainedUpstreamModelFormats(u, result.Models, result.modelFormats, result.ValidationComplete)
	if len(result.Models) > 0 {
		result.OK = true
		if !result.ValidationComplete {
			result.ModelsAvailable = len(result.Models)
		}
	}
	if err := s.recordUpstreamModels(ctx, store, u, result.Models, result.modelFormats, result.ErrorCode, result.ValidationComplete); err != nil {
		return nil, mapRepoErr(err)
	}
	return &result, nil
}

// ValidateAllUpstreams performs the same complete model validation used by the
// add/edit flow for every saved upstream. Each row is checked independently so
// one broken relay cannot abort the rest of the operation; the returned order
// is stable by upstream ID and every completed probe records health telemetry.
func (s *Service) ValidateAllUpstreams(ctx context.Context) (*UpstreamValidationSummary, error) {
	return s.validateAllUpstreams(ctx, nil)
}

// ValidateAllUpstreamsWithProgress is the asynchronous-management building
// block.  The legacy ValidateAllUpstreams method remains synchronous for
// existing callers; the handler uses this variant from a background task and
// exposes the snapshots through a small polling endpoint.
func (s *Service) ValidateAllUpstreamsWithProgress(ctx context.Context, report UpstreamValidationProgressFunc) (*UpstreamValidationSummary, error) {
	return s.validateAllUpstreams(ctx, report)
}

func (s *Service) validateAllUpstreams(ctx context.Context, report UpstreamValidationProgressFunc) (*UpstreamValidationSummary, error) {
	if s == nil {
		return nil, errUpstreamStoreUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	validationCtx, validationCancel := context.WithTimeout(ctx, upstreamValidationBatchTimeout)
	defer validationCancel()
	releaseValidation, err := s.lockUpstreamValidation(validationCtx)
	if err != nil {
		return nil, err
	}
	defer releaseValidation()
	store, err := s.upstreamStore()
	if err != nil {
		return nil, err
	}
	// A canceled HTTP request must not make the production snapshot query fail
	// before we can return the same explicit `canceled` items that the worker
	// path reports. The detached read is bounded and is used only when the
	// caller was already canceled before this operation started; normal in-flight
	// cancellation still propagates to the database and probe requests.
	snapshotCtx := validationCtx
	var snapshotCancel context.CancelFunc
	if validationCtx.Err() != nil {
		snapshotCtx, snapshotCancel = context.WithTimeout(context.Background(), upstreamValidationSnapshotTimeout)
		defer snapshotCancel()
	}
	rows, err := loadUpstreamValidationSnapshot(snapshotCtx, store)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	items := make([]UpstreamValidationItem, len(rows))
	emit := func(progress UpstreamValidationProgress) {
		if report != nil {
			report(progress)
		}
	}
	// The initial total is available as soon as the stable snapshot is read;
	// model totals are filled as each upstream returns its catalogue.
	emit(UpstreamValidationProgress{UpstreamsTotal: len(rows)})
	if len(rows) == 0 {
		result := &UpstreamValidationSummary{Items: []UpstreamValidationItem{}, DurationMS: time.Since(started).Milliseconds()}
		emit(UpstreamValidationProgress{Done: true})
		return result, nil
	}
	workers := upstreamValidationConcurrency
	if len(rows) < workers {
		workers = len(rows)
	}
	type job struct {
		index int
		row   *domain.Upstream
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	var progressMu sync.Mutex
	var progress UpstreamValidationProgress
	progress.UpstreamsTotal = len(rows)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				item := s.validateStoredUpstream(validationCtx, store, j.row)
				items[j.index] = item
				progressMu.Lock()
				progress.UpstreamsChecked++
				progress.ModelsTotal += item.ModelsTotal
				progress.ModelsChecked += item.ModelsChecked
				progress.ModelsAvailable += item.ModelsAvailable
				progress.ModelsFailed += item.ModelsFailed
				current := progress
				progressMu.Unlock()
				emit(current)
			}
		}()
	}
	dispatchCanceled := false
dispatch:
	for i, row := range rows {
		// Check before entering the two-way select. When the caller is already
		// canceled and a worker is ready, both cases would otherwise be
		// selectable and Go may hand out one extra job nondeterministically.
		// A pre-canceled batch must not start any upstream probe.
		if validationCtx.Err() != nil {
			dispatchCanceled = true
			close(jobs)
			break dispatch
		}
		select {
		case <-validationCtx.Done():
			// Stop handing out new rows, then drain jobs already accepted by
			// workers. A select may choose the send case even after cancellation
			// when both cases are ready, so do not classify by the loop index;
			// use each item's Attempted marker after the workers have joined.
			dispatchCanceled = true
			close(jobs)
			break dispatch
		case jobs <- job{index: i, row: row}:
		}
	}
	if !dispatchCanceled {
		close(jobs)
	}
	wg.Wait()
	if dispatchCanceled {
		code := upstreamValidationContextErrorCode(validationCtx)
		for i := range items {
			if !items[i].Attempted {
				items[i] = UpstreamValidationItem{Upstream: rows[i], ErrorCode: code}
			}
		}
	}
	result := summarizeUpstreamValidation(items, started)
	progressMu.Lock()
	finalProgress := progress
	progressMu.Unlock()
	finalProgress.Done = true
	emit(finalProgress)
	return result, nil
}

func upstreamValidationContextErrorCode(ctx context.Context) string {
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "canceled"
}

// validationContextErrorCode returns a diagnostic only when the context has
// actually ended. It is kept separate from upstreamValidationContextErrorCode,
// whose callers already know that cancellation occurred and therefore use its
// "canceled" fallback for all non-deadline cases.
func validationContextErrorCode(ctx context.Context) string {
	if ctx == nil || ctx.Err() == nil {
		return ""
	}
	return upstreamValidationContextErrorCode(ctx)
}

func loadUpstreamValidationSnapshot(ctx context.Context, store UpstreamStore) ([]*domain.Upstream, error) {
	if snapshot, ok := store.(UpstreamSnapshotStore); ok {
		rows, err := snapshot.ListAllUpstreams(ctx)
		if err != nil {
			return nil, err
		}
		if len(rows) > upstreamValidationSnapshotMax {
			return nil, fmt.Errorf("%w: upstream validation snapshot exceeds %d rows", ErrInvalidInput, upstreamValidationSnapshotMax)
		}
		return normalizeUpstreamSnapshotStrict(rows)
	}

	// Older integrations expose only paginated reads. Fetch the first page to
	// learn the current count, then request the complete set from offset zero.
	// Unlike walking OFFSET pages, a single ordered read cannot skip a row merely
	// because another request inserted or removed an item between pages. The
	// final ID de-duplication also protects integrations that return an unstable
	// count while they are being updated.
	first, total, err := store.ListUpstreams(ctx, repository.ListQuery{
		Limit: upstreamValidationBatchLimit, Sort: "id", Order: "asc",
	})
	if err != nil {
		return nil, err
	}
	if total < 0 {
		return nil, fmt.Errorf("%w: upstream count is invalid", ErrInvalidInput)
	}
	if total <= int64(len(first)) {
		if int64(len(first)) != total {
			return nil, fmt.Errorf("%w: upstream validation count changed while loading", repository.ErrConflict)
		}
		return normalizeUpstreamSnapshotStrict(first)
	}
	if total > upstreamValidationSnapshotMax {
		return nil, fmt.Errorf("%w: upstream validation snapshot exceeds %d rows", ErrInvalidInput, upstreamValidationSnapshotMax)
	}
	limit := int(total)
	all, _, err := store.ListUpstreams(ctx, repository.ListQuery{Limit: limit, Sort: "id", Order: "asc"})
	if err != nil {
		return nil, err
	}
	if int64(len(all)) != total {
		return nil, fmt.Errorf("%w: upstream validation snapshot changed while loading", ErrConflict)
	}
	return normalizeUpstreamSnapshotStrict(all)
}

func normalizeUpstreamSnapshot(rows []*domain.Upstream) []*domain.Upstream {
	ordered, err := normalizeUpstreamSnapshotStrict(rows)
	if err != nil {
		return nil
	}
	return ordered
}

// normalizeUpstreamSnapshotStrict preserves the all-items contract. A duplicate
// or malformed row indicates that the backing store did not provide a coherent
// snapshot; silently de-duplicating it would make the UI report success while a
// provider was never checked.
func normalizeUpstreamSnapshotStrict(rows []*domain.Upstream) ([]*domain.Upstream, error) {
	if len(rows) == 0 {
		return []*domain.Upstream{}, nil
	}
	byID := make(map[int64]*domain.Upstream, len(rows))
	for _, row := range rows {
		if row == nil || row.ID <= 0 {
			return nil, fmt.Errorf("%w: upstream validation snapshot contains an invalid row", ErrInvalidInput)
		}
		if _, exists := byID[row.ID]; exists {
			return nil, fmt.Errorf("%w: upstream validation snapshot contains duplicate id=%d", repository.ErrConflict, row.ID)
		}
		byID[row.ID] = row
	}
	ordered := make([]*domain.Upstream, 0, len(byID))
	for _, row := range byID {
		ordered = append(ordered, row)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].ID < ordered[j].ID
	})
	return ordered, nil
}

func (s *Service) validateStoredUpstream(ctx context.Context, store UpstreamStore, expected *domain.Upstream) UpstreamValidationItem {
	item := UpstreamValidationItem{Upstream: expected, Attempted: true}
	if expected == nil || expected.ID <= 0 {
		item.ErrorCode = "invalid_value"
		return item
	}
	base := normalizeUpstreamBaseURL(expected.BaseURL)
	if err := validateBaseURL(base); err != nil {
		item.ErrorCode = "invalid_value"
		return item
	}
	// The catalogue validator derives its deadline from the number of advertised
	// models.  Keep the outer row deadline at the batch cap so it does not
	// silently reintroduce the legacy 30-second cutoff for larger catalogues.
	probeCtx, cancel := context.WithTimeout(ctx, upstreamValidationBatchTimeout)
	defer cancel()
	client := s.managementHTTPClient()
	client.Timeout = upstreamModelValidationPerModel + time.Second
	client.CheckRedirect = upstreamCheckRedirect
	started := time.Now()
	result := validateUpstreamModels(probeCtx, client, base, normalizeUpstreamKey(derefUpstreamKey(expected.UpstreamKey)))
	item.LatencyMS = time.Since(started).Milliseconds()
	result.Models = retainedUpstreamModels(expected, result.Models, result.advertisedModels, result.transientModels, result.ValidationComplete, result.ErrorCode)
	result.modelFormats = retainedUpstreamModelFormats(expected, result.Models, result.modelFormats, result.ValidationComplete)
	if len(result.Models) > 0 {
		result.OK = true
		if !result.ValidationComplete {
			result.ModelsAvailable = len(result.Models)
		}
	}
	item.Models = append([]string{}, result.Models...)
	item.ModelsTotal = result.ModelsTotal
	item.ModelsChecked = result.ModelsChecked
	item.ModelsAvailable = result.ModelsAvailable
	item.ModelsFailed = result.ModelsFailed
	item.ValidationComplete = result.ValidationComplete
	// A partial run with at least one current or retained model is still usable.
	// `ValidationComplete` remains false so the UI can show a retry warning, but
	// the upstream must not be presented as wholly unavailable.
	item.OK = result.OK
	item.ErrorCode = result.ErrorCode
	if item.ErrorCode == "" && !item.OK {
		item.ErrorCode = "model_unavailable"
	}
	// A run with confirmed models writes that subset even if other advertised
	// models were not completed; a run with no confirmed model leaves the prior
	// snapshot in place.
	persistCtx, persistCancel := upstreamValidationPersistenceContext(ctx)
	defer persistCancel()
	if err := s.recordUpstreamModels(persistCtx, store, expected, result.Models, result.modelFormats, result.ErrorCode, result.ValidationComplete); err != nil {
		item.OK = false
		if errors.Is(err, repository.ErrConflict) {
			item.ErrorCode = "superseded"
			if latest, getErr := store.GetUpstream(persistCtx, expected.ID); getErr == nil {
				item.Upstream = latest
			}
			return item
		}
		item.ErrorCode = upstreamValidationPersistenceErrorCode(ctx, item.ErrorCode)
		if s.log != nil {
			s.log.Warn("upstream model snapshot not saved", logx.Int64("id", expected.ID), logx.Error(err))
		}
		return item
	}
	var probeErr *string
	if item.ErrorCode != "" {
		probeErr = &item.ErrorCode
	}
	updated, err := store.RecordUpstreamProbe(persistCtx, expected, item.OK, item.LatencyMS, probeErr)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			item.ErrorCode = "superseded"
			if latest, getErr := store.GetUpstream(persistCtx, expected.ID); getErr == nil {
				item.Upstream = latest
			}
			return item
		}
		item.OK = false
		item.ErrorCode = upstreamValidationPersistenceErrorCode(ctx, item.ErrorCode)
		if s.log != nil {
			s.log.Warn("upstream validation probe not saved", logx.Int64("id", expected.ID), logx.Error(err))
		}
		return item
	}
	item.Upstream = updated
	return item
}

// upstreamValidationPersistenceContext carries validation diagnostics through
// a client disconnect without allowing the database writes to outlive a short
// bounded window. WithoutCancel also preserves request-scoped values used by
// repository instrumentation while removing its cancellation signal.
func upstreamValidationPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), upstreamValidationPersistenceTimeout)
}

// upstreamValidationPersistenceErrorCode keeps the caller-facing outcome
// meaningful when a client disconnects while the bounded diagnostic write is
// finishing. A canceled or deadline-exceeded request must not be relabeled as
// a generic storage failure; an active request still reports storage errors
// normally.
func upstreamValidationPersistenceErrorCode(ctx context.Context, current string) string {
	if current == "canceled" || current == "timeout" {
		return current
	}
	if ctx != nil {
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return "canceled"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return "timeout"
		}
	}
	return "storage"
}

func summarizeUpstreamValidation(items []UpstreamValidationItem, started time.Time) *UpstreamValidationSummary {
	summary := &UpstreamValidationSummary{
		Total: len(items), Items: items, DurationMS: time.Since(started).Milliseconds(),
	}
	for _, item := range items {
		// Attempted means only that a worker accepted the row. A canceled or
		// timed-out validation may therefore be attempted without having checked
		// the complete advertised catalogue. Report completed rows only when the
		// capability result is complete; incomplete details remain visible in
		// Items and can be retried without inflating the summary counters.
		if !item.Attempted || !item.ValidationComplete {
			continue
		}
		summary.Completed++
		if item.OK {
			summary.Passed++
		} else {
			summary.Failed++
		}
	}
	return summary
}

func (s *Service) recordUpstreamModels(ctx context.Context, store UpstreamStore, expected *domain.Upstream, models []string, modelFormats map[string][]domain.RequestFormat, code string, complete bool) error {
	recorder, ok := store.(UpstreamModelStore)
	if !ok {
		return nil
	}
	var modelErr *string
	if code != "" {
		modelErr = &code
		if !complete && len(models) == 0 {
			// Authentication is authoritative even when the catalogue request
			// stopped early: clear stale routes for a key that is no longer valid.
			// Other incomplete failures retain the previous snapshot.
			if code != "auth" && code != "model_unavailable" {
				models = nil
				modelFormats = nil
			} else {
				models = []string{}
				modelFormats = map[string][]domain.RequestFormat{}
			}
		}
	}
	var err error
	if capabilityRecorder, capabilityOK := store.(UpstreamModelCapabilityStore); capabilityOK {
		_, err = capabilityRecorder.RecordUpstreamModelCapabilities(ctx, expected, models, modelFormats, modelErr)
	} else {
		_, err = recorder.RecordUpstreamModels(ctx, expected, models, modelErr)
	}
	if err != nil {
		// A concurrent endpoint/key edit makes the probe stale. The read result is
		// still useful to the caller, while the next reload will use the new config.
		return err
	}
	s.invalidateUpstreamConfig(ctx)
	return nil
}

func retainedUpstreamModelFormats(expected *domain.Upstream, models []string, current map[string][]domain.RequestFormat, complete bool) map[string][]domain.RequestFormat {
	out := make(map[string][]domain.RequestFormat, len(models))
	for _, model := range models {
		if formats := current[model]; len(formats) > 0 {
			out[model] = append([]domain.RequestFormat(nil), formats...)
			continue
		}
		if !complete && expected != nil {
			if formats := expected.ModelFormats[model]; len(formats) > 0 {
				out[model] = append([]domain.RequestFormat(nil), formats...)
			}
		}
	}
	return out
}

func cloneModelFormatSnapshot(in map[string][]domain.RequestFormat) map[string][]domain.RequestFormat {
	out := make(map[string][]domain.RequestFormat, len(in))
	for model, formats := range in {
		out[model] = append([]domain.RequestFormat(nil), formats...)
	}
	return out
}

// retainedUpstreamModels merges a previous verified snapshot only for an
// unchanged upstream and a non-authoritative, incomplete run. This prevents a
// timeout, relay reset, or malformed catalogue from deleting models that a
// manual request already proved usable. Authentication and completed
// model-specific failures remain authoritative and publish the current result.
func retainedUpstreamModels(expected *domain.Upstream, current, advertised, transient []string, complete bool, code string) []string {
	if expected == nil || len(expected.Models) == 0 || !isRetainableValidation(code, complete) {
		return append([]string{}, current...)
	}
	previous := make([]string, 0, len(expected.Models))
	advertisedSet := make(map[string]struct{}, len(advertised))
	for _, model := range advertised {
		model = strings.TrimSpace(model)
		if model != "" {
			advertisedSet[model] = struct{}{}
		}
	}
	transientSet := make(map[string]struct{}, len(transient))
	for _, model := range transient {
		model = strings.TrimSpace(model)
		if model != "" {
			transientSet[model] = struct{}{}
		}
	}
	for _, model := range expected.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		// If the catalogue was read successfully, retain only an advertised
		// model whose probe failed transiently. A definitive model/auth failure
		// must be allowed to remove that route. When the catalogue itself was
		// interrupted, there is no trustworthy advertised set, so the last
		// snapshot remains the fallback as a whole.
		if len(advertisedSet) > 0 {
			if _, listed := advertisedSet[model]; !listed {
				if complete {
					continue
				}
				previous = append(previous, model)
				continue
			}
			if _, retryable := transientSet[model]; !retryable {
				continue
			}
		} else if complete {
			continue
		}
		previous = append(previous, model)
	}
	return mergeUpstreamModelLists(previous, current)
}

func isRetainableValidation(code string, complete bool) bool {
	code = strings.TrimSpace(code)
	if complete {
		return isTransientModelValidationCode(code)
	}
	return isRetainableIncompleteValidationCode(code)
}

func isRetainableIncompleteValidationCode(code string) bool {
	switch strings.TrimSpace(code) {
	case "canceled", "timeout", "network", "upstream", "rate_limited", "invalid_value", "http_error":
		return true
	default:
		return false
	}
}

func mergeUpstreamModelLists(previous, current []string) []string {
	seen := make(map[string]struct{}, len(previous)+len(current))
	merged := make([]string, 0, len(previous)+len(current))
	for _, source := range [][]string{previous, current} {
		for _, model := range source {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if _, exists := seen[model]; exists {
				continue
			}
			seen[model] = struct{}{}
			merged = append(merged, model)
		}
	}
	return merged
}

// PreviewUpstreamModels validates a not-yet-saved endpoint and key. It is used
// by the create form before persistence, so a typo or unsupported credential
// cannot create an unusable upstream record.
func (s *Service) PreviewUpstreamModels(ctx context.Context, base, key string) (*UpstreamModelsResult, error) {
	releaseValidation, err := s.lockUpstreamValidation(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseValidation()
	if ctx == nil {
		ctx = context.Background()
	}
	base = normalizeUpstreamBaseURL(base)
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}
	client := s.managementHTTPClient()
	client.Timeout = upstreamModelValidationPerModel + time.Second
	client.CheckRedirect = upstreamCheckRedirect
	result := validateUpstreamModels(ctx, client, base, normalizeUpstreamKey(key))
	return &result, nil
}

// TestUpstream keeps the existing programmatic API and uses the first real
// model for callers that do not need to choose one explicitly.
func (s *Service) TestUpstream(ctx context.Context, id int64) (*UpstreamProbeResult, error) {
	return s.TestUpstreamWithModel(ctx, id, "")
}

// TestUpstreamWithModel sends one tiny, non-streaming "hi" request through the
// same configured transport used by the gateway. When no model is supplied,
// the first entry from the current /v1/models catalogue is used. An explicit
// model is sent directly because some relays hide tenant aliases from that
// catalogue. Responses-compatible relays are tried first; a format rejection
// falls back to Chat Completions.
func (s *Service) TestUpstreamWithModel(ctx context.Context, id int64, requestedModel string) (*UpstreamProbeResult, error) {
	releaseValidation, err := s.lockUpstreamValidation(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseValidation()
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
	base := normalizeUpstreamBaseURL(u.BaseURL)
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}
	client := s.managementHTTPClient()
	client.Timeout = 12 * time.Second
	client.CheckRedirect = upstreamCheckRedirect
	key := normalizeUpstreamKey(derefUpstreamKey(u.UpstreamKey))
	model := strings.TrimSpace(requestedModel)
	if model == "" {
		// Discovery is the source for a safe default when the caller did not
		// choose a model. An explicit model is tested directly below: relays may
		// hide tenant aliases or transiently fail /models while accepting that
		// identifier on the completion route.
		// Discovery and the actual model request are separate operations.  Give
		// each its own bounded context so a slow but valid /models response does
		// not leave the completion probe with only the tail of the same budget.
		discoveryCtx, discoveryCancel := context.WithTimeout(ctx, upstreamManualModelTestTimeout)
		models, modelCode := fetchAdvertisedModels(discoveryCtx, client, base, key)
		discoveryCancel()
		if modelCode != "" {
			return s.recordUpstreamTestFailure(ctx, store, u, modelCode)
		}
		if len(models) == 0 {
			return s.recordUpstreamTestFailure(ctx, store, u, "model_unavailable")
		}
		model = models[0]
	}
	// The live catalogue is the default source for the picker, but it is not a
	// complete authority for an explicit management test. Some relays hide
	// aliases, tenant-scoped models, or newly enabled models from /models while
	// accepting the same identifier on the completion route. When an operator
	// explicitly supplies a model, send the real request and let the provider's
	// response decide whether it is usable; this keeps a manually successful
	// model from being rejected solely because discovery omitted it.
	started := time.Now()
	probeCtx, probeCancel := context.WithTimeout(ctx, upstreamManualModelTestTimeout)
	defer probeCancel()
	status, format, requestErr := sendUpstreamModelProbeWithRetryFormat(probeCtx, client, base, key, model)
	latency := time.Since(started).Milliseconds()
	ok := requestErr == nil && status >= 200 && status < 300
	code := classifyUpstreamTestError(probeCtx, status, requestErr)
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
	if ok {
		// A selected model can be hidden from /v1/models (tenant aliases and
		// newly enabled models are common examples). Persist a successful manual
		// probe so group pickers and later transient validations do not lose the
		// exact model that the operator just proved usable.
		persisted, persistErr := s.recordExplicitUpstreamModel(ctx, store, updatedOrFallbackUpstream(updated, u), model, format)
		if persistErr != nil {
			if errors.Is(persistErr, repository.ErrConflict) {
				return supersededUpstreamResult(ctx, store, id)
			}
			return nil, mapRepoErr(persistErr)
		}
		if persisted != nil {
			updated = persisted
		}
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
	key = normalizeUpstreamKey(key)
	// OpenAI-compatible relays usually expose /v1/models, but a number of
	// gateways expose the same catalogue at /models when their configured base
	// URL already represents the versioned API root. Try that route only when
	// the canonical route explicitly looks missing; auth, network, rate-limit,
	// and provider failures must retain their original classification.
	paths := []string{"/v1/models", "/models"}
	lastCode := "http_error"
	for i, path := range paths {
		models, code, status, retryable := fetchAdvertisedModelsPagedPath(ctx, client, upstreamURL(base, path), key)
		if isTransientModelValidationCode(code) && ctx.Err() == nil {
			if waitForUpstreamCatalogueRetry(ctx) {
				models, code, status, retryable = fetchAdvertisedModelsPagedPath(ctx, client, upstreamURL(base, path), key)
			}
		}
		if code == "" {
			return models, ""
		}
		lastCode = code
		if i == len(paths)-1 || !retryable || status == 0 {
			if code == "model_unavailable" && models != nil {
				return models, code
			}
			return nil, code
		}
	}
	return nil, lastCode
}

func waitForUpstreamCatalogueRetry(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	timer := time.NewTimer(upstreamModelCatalogueRetryDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

// advertisedModelsPage is one response from a model catalogue. Some relays
// expose a cursor-paginated list even though the OpenAI-compatible contract
// usually returns one complete page. The cursor is followed only after the
// page itself has passed the same strict model-entry validation.
type advertisedModelsPage struct {
	models     []string
	recognized bool
	hasMore    bool
	nextURL    string
	nextCursor string
	cursorKey  string
}

// fetchAdvertisedModelsPagedPath reads one catalogue route, following a
// bounded same-origin cursor chain. The boolean return identifies route-level
// failures for which the caller may try a compatibility path; it deliberately
// excludes model/auth/provider errors and failures on later pages.
func fetchAdvertisedModelsPagedPath(ctx context.Context, client *http.Client, target, key string) ([]string, string, int, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	current := target
	visited := make(map[string]struct{}, 1)
	all := make([]string, 0)
	seen := make(map[string]struct{})
	lastStatus := 0
	for page := 0; page < upstreamModelCatalogueMaxPages; page++ {
		if _, exists := visited[current]; exists {
			return nil, "invalid_value", lastStatus, false
		}
		visited[current] = struct{}{}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		pageResult, code, status, retryable := fetchAdvertisedModelsPage(requestCtx, client, current, key)
		cancel()
		lastStatus = status
		if code != "" {
			// A compatibility path is meaningful only for the first request. A
			// later page failure means the catalogue is incomplete and must not
			// publish the prefix that happened to arrive first.
			if page > 0 {
				retryable = false
			}
			return nil, code, status, retryable
		}
		if !pageResult.recognized {
			return nil, "invalid_value", status, false
		}
		if len(all)+len(pageResult.models) > upstreamModelCatalogueMax {
			return nil, "invalid_value", status, false
		}
		for _, model := range pageResult.models {
			if _, exists := seen[model]; exists {
				continue
			}
			seen[model] = struct{}{}
			all = append(all, model)
		}

		next := strings.TrimSpace(pageResult.nextURL)
		if next == "" && pageResult.hasMore && pageResult.nextCursor != "" {
			param := pageResult.cursorKey
			if param == "" {
				param = "after"
			}
			var ok bool
			next, ok = appendAdvertisedModelsCursor(current, param, pageResult.nextCursor)
			if !ok {
				return nil, "invalid_value", status, false
			}
		}
		if next == "" {
			if pageResult.hasMore {
				// A declared next page without a cursor/link cannot be fetched
				// safely; treating the first page as complete would hide models.
				return nil, "invalid_value", status, false
			}
			if len(all) == 0 {
				// An explicitly valid but empty catalogue is a completed
				// capability check. Keep it distinct from malformed JSON so
				// callers clear a stale model snapshot instead of continuing to
				// route to old models.
				return []string{}, "model_unavailable", status, false
			}
			return all, "", status, false
		}
		resolved, ok := resolveAdvertisedModelsNextURL(current, next)
		if !ok {
			return nil, "invalid_value", status, false
		}
		current = resolved
	}
	return nil, "invalid_value", lastStatus, false
}

// fetchAdvertisedModelsPath preserves the historical one-request helper for
// tests and integrations that call it internally. A paginated response is
// intentionally reported invalid here; fetchAdvertisedModels uses the bounded
// paged variant above to retrieve the complete catalogue.
func fetchAdvertisedModelsPath(ctx context.Context, client *http.Client, target, key string) ([]string, string, int, bool) {
	page, code, status, retryable := fetchAdvertisedModelsPage(ctx, client, target, key)
	if code != "" {
		return nil, code, status, retryable
	}
	if !page.recognized || page.hasMore || page.nextURL != "" || page.nextCursor != "" {
		return nil, "invalid_value", status, false
	}
	if len(page.models) == 0 {
		return []string{}, "model_unavailable", status, false
	}
	return page.models, "", status, false
}

// fetchAdvertisedModelsPage performs one bounded catalogue request. The
// boolean return identifies route-level failures for which the caller may try
// a compatibility path; it deliberately excludes model/auth/provider errors.
func fetchAdvertisedModelsPage(ctx context.Context, client *http.Client, target, key string) (advertisedModelsPage, string, int, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Catalogue reads are free GETs, so an auth-header mismatch can be retried
	// safely with the other conventional header.  Keep the retry inside this
	// helper so cursor pages and the /models compatibility path share exactly
	// the same behavior.
	modes := upstreamAuthModes(key, upstreamAuthBearer)
	var last advertisedModelsPage
	var lastCode string
	var lastStatus int
	var lastRetryable bool
	for index, mode := range modes {
		page, code, status, retryable, authRetryable := fetchAdvertisedModelsPageWithAuthMode(ctx, client, target, key, mode)
		last, lastCode, lastStatus, lastRetryable = page, code, status, retryable
		if code == "" || index == len(modes)-1 || code != "auth" || !authRetryable {
			return page, code, status, retryable
		}
	}
	return last, lastCode, lastStatus, lastRetryable
}

func fetchAdvertisedModelsPageWithAuth(ctx context.Context, client *http.Client, target, key string, mode upstreamAuthMode) (advertisedModelsPage, string, int, bool) {
	page, code, status, retryable, _ := fetchAdvertisedModelsPageWithAuthMode(ctx, client, target, key, mode)
	return page, code, status, retryable
}

func fetchAdvertisedModelsPageWithAuthMode(ctx context.Context, client *http.Client, target, key string, mode upstreamAuthMode) (advertisedModelsPage, string, int, bool, bool) {
	var empty advertisedModelsPage
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return empty, "invalid_value", 0, false, false
	}
	setUpstreamAuthHeader(req, key, mode)
	resp, err := client.Do(req)
	if err != nil || resp == nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return empty, "timeout", 0, false, false
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return empty, "canceled", 0, false, false
		}
		return empty, "network", 0, false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Keep a small bounded copy for 404 classification. Some gateways use a
		// JSON error envelope on a missing model/tenant; treating that as a
		// missing route would trigger a second request against the compatibility
		// path and can duplicate a charge. Plain-text/HTML router pages remain
		// route-level signals and keep the fallback behavior.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			authRetryable := shouldRetryUpstreamAuthMode(resp.StatusCode, &upstreamHTTPError{status: resp.StatusCode, body: body}, mode)
			return empty, "auth", resp.StatusCode, false, authRetryable
		case http.StatusTooManyRequests:
			return empty, "rate_limited", resp.StatusCode, false, false
		case http.StatusNotFound:
			return empty, "http_error", resp.StatusCode, !isStructuredProviderFailure(body), false
		case http.StatusMethodNotAllowed, http.StatusNotImplemented:
			return empty, "http_error", resp.StatusCode, true, false
		default:
			if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 {
				return empty, "upstream", resp.StatusCode, false, false
			}
			return empty, "http_error", resp.StatusCode, false, false
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return empty, "network", resp.StatusCode, false, false
	}
	if len(body) > 1<<20 {
		return empty, "invalid_value", resp.StatusCode, false, false
	}
	page, recognized := parseUpstreamModelsPagePayload(body)
	if !recognized {
		if code, ok := classifyModelCatalogueErrorEnvelope(body); ok {
			return empty, code, resp.StatusCode, false, false
		}
		return empty, "invalid_value", resp.StatusCode, false, false
	}
	return page, "", resp.StatusCode, false, false
}

// classifyModelCatalogueErrorEnvelope handles relays that return an
// OpenAI-style error object with HTTP 200 for a failed /models request. It is
// deliberately limited to a top-level error member; arbitrary JSON remains an
// invalid catalogue instead of being guessed as an auth or outage signal.
func classifyModelCatalogueErrorEnvelope(body []byte) (string, bool) {
	body = trimUpstreamJSONBody(body)
	var value map[string]json.RawMessage
	if err := json.Unmarshal(body, &value); err != nil {
		return "", false
	}
	if raw, ok := value["error"]; !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	return classifySuccessfulErrorEnvelope(string(body)), true
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
	advertisedModels   []string
	transientModels    []string
	modelFormats       map[string][]domain.RequestFormat
}

// validateUpstreamModels turns an advertised catalogue into a verified
// catalogue. Every model receives one tiny non-streaming request, bounded by a
// worker pool and per-model/overall deadlines. The route is Responses first;
// only an explicit protocol/format rejection is retried as Chat Completions.
func validateUpstreamModels(ctx context.Context, client *http.Client, base, key string) UpstreamModelsResult {
	if ctx == nil {
		ctx = context.Background()
	}
	// Catalogue acquisition has its own deadline. Without this boundary a
	// cursor chain (each page has a ten-second request budget) could consume
	// the entire model-validation window before a single model is probed.
	catalogueCtx, catalogueCancel := context.WithTimeout(ctx, upstreamModelCatalogueTimeout)
	models, code := fetchAdvertisedModels(catalogueCtx, client, base, key)
	catalogueCancel()
	if code != "" {
		if code == "model_unavailable" && models != nil {
			return UpstreamModelsResult{
				Models:             models,
				OK:                 false,
				ErrorCode:          code,
				ModelsTotal:        len(models),
				ModelsChecked:      0,
				ModelsAvailable:    0,
				ModelsFailed:       0,
				ValidationComplete: true,
				advertisedModels:   append([]string{}, models...),
			}
		}
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
		advertisedModels:   append([]string{}, models...),
		transientModels:    append([]string{}, result.transientModels...),
		modelFormats:       cloneModelFormatSnapshot(result.modelFormats),
	}
}

func validateModelCatalogue(ctx context.Context, client *http.Client, base, key string, models []string) upstreamModelValidation {
	// The default deadline must cover every model when probes are serialized.
	// Keep the historical 30s floor for small catalogues, then add one second
	// of hand-off/response slack per catalogue wave so a valid large catalogue
	// is not cut off solely because it has more models than the floor allows.
	return validateModelCatalogueWithTimeout(ctx, client, base, key, models, modelValidationTimeoutForCount(len(models)))
}

// modelValidationTimeoutForCount keeps the batch budget proportional to the
// amount of work while retaining a floor for the common one- or two-model
// case.  The hard batch cap prevents a provider that advertises an enormous
// catalogue from holding the validation lock forever.
func modelValidationTimeoutForCount(modelCount int) time.Duration {
	if modelCount <= 0 {
		return upstreamModelValidationTimeout
	}
	waves := (modelCount + upstreamModelValidationConcurrency - 1) / upstreamModelValidationConcurrency
	timeout := upstreamModelValidationTimeout
	minimum := time.Duration(waves)*upstreamModelValidationPerModel + time.Second
	if minimum > timeout {
		timeout = minimum
	}
	if timeout > upstreamValidationBatchTimeout {
		timeout = upstreamValidationBatchTimeout
	}
	return timeout
}

// validateModelCatalogueWithTimeout contains the bounded catalogue worker and
// accepts a timeout override so the deadline boundary can be tested without
// waiting for the production 30-second limit.
func validateModelCatalogueWithTimeout(ctx context.Context, client *http.Client, base, key string, models []string, timeout time.Duration) upstreamModelValidation {
	if ctx == nil {
		ctx = context.Background()
	}
	models = dedupeProbeModels(models)
	if len(models) == 0 {
		return upstreamModelValidation{ValidationComplete: true}
	}
	validationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type outcome struct {
		model   string
		format  domain.RequestFormat
		ok      bool
		checked bool
		code    string
	}
	results := make(chan outcome, len(models))
	workers := upstreamModelValidationConcurrency
	if len(models) < workers {
		workers = len(models)
	}
	jobs := make(chan string, workers)
	var wg sync.WaitGroup
	var stopDispatchOnce sync.Once
	stopDispatch := make(chan struct{})
	// An authentication failure is account-wide and is safe to stop on after
	// the current worker batch. Rate limits, provider 5xx responses, and network
	// resets can be model-specific or transient; dispatching must continue so a
	// single bad model cannot hide other usable models from the verified snapshot.
	recordFatal := func(code string) {
		if !isFatalModelValidationCode(code) {
			return
		}
		stopDispatchOnce.Do(func() { close(stopDispatch) })
	}
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
				status, format, requestErr := sendUpstreamModelProbeWithRetryFormat(modelCtx, client, base, key, model)
				modelCancel()
				modelCode := classifyModelValidationError(validationCtx, status, requestErr)
				// A 401/403 can be scoped to the selected model (for example a
				// relay exposes a catalogue broader than the credential's
				// entitlement). Keep probing the remaining models in that case;
				// only an account-wide auth response should stop dispatch.
				if !(modelCode == "auth" && isModelScopedAuthFailure(requestErr)) {
					recordFatal(modelCode)
				}
				results <- outcome{model: model, format: format, ok: modelCode == "", checked: true, code: modelCode}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, model := range models {
			// Check the stop signal before entering the send select. When both a
			// worker and the stop channel are ready, Go may otherwise choose the
			// send case repeatedly; the explicit check keeps post-fatal dispatch
			// to at most one in-flight handoff.
			select {
			case <-validationCtx.Done():
				return
			case <-stopDispatch:
				return
			default:
			}
			select {
			case <-validationCtx.Done():
				return
			case <-stopDispatch:
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
	transientSet := make(map[string]struct{})
	modelFormats := make(map[string][]domain.RequestFormat)
	for outcome := range results {
		if outcome.checked {
			checked++
		}
		if outcome.ok {
			valid = append(valid, outcome.model)
			if outcome.format.Valid() {
				modelFormats[outcome.model] = []domain.RequestFormat{outcome.format}
			}
		} else if outcome.checked {
			failed++
			if outcome.code != "" {
				counts[outcome.code]++
				if isTransientModelValidationCode(outcome.code) {
					transientSet[outcome.model] = struct{}{}
				}
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
	transientModels := make([]string, 0, len(transientSet))
	for _, model := range models {
		if _, ok := transientSet[model]; ok {
			transientModels = append(transientModels, model)
		}
	}
	// An account-wide authentication signal can stop dispatching new work. It
	// must not turn a run that already checked every advertised model into a
	// partial result (for example a one-model catalogue returning auth). A caller
	// deadline/cancellation or this function's own bounded deadline makes an
	// otherwise complete-looking run incomplete.
	complete := checked == len(models) && validationCtx.Err() == nil
	// Any transient transport/throttling failure means this is not a stable
	// capability snapshot, even when other models succeeded. Keeping the run
	// explicitly incomplete lets persistence retain the previous verified model
	// while still exposing the newly confirmed subset. Otherwise a single 429 or
	// timeout would make a previously manual-tested model disappear.
	// Authentication/model-specific failures remain authoritative when every
	// advertised model was actually checked.
	if complete && hasTransientValidationFailure(counts) {
		complete = false
	}
	validationCode := ""
	if !complete {
		// A partial run is not a capability snapshot. Even if some workers
		// succeeded, keep the previous verified catalogue and require a retry;
		// publishing a partial list would make the scheduler silently hide models
		// that were never checked.
		switch {
		// The caller's cancellation is the most useful explanation to the
		// management API. A worker may have observed an auth/429 response just
		// before the request was canceled; do not let that incidental result win.
		case errors.Is(ctx.Err(), context.Canceled):
			validationCode = "canceled"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			validationCode = "timeout"
		case errors.Is(validationCtx.Err(), context.Canceled):
			validationCode = "canceled"
		case errors.Is(validationCtx.Err(), context.DeadlineExceeded):
			validationCode = "timeout"
		default:
			validationCode = dominantModelValidationError(counts)
		}
	} else if failed > 0 {
		// A complete run can safely publish the verified subset while explaining
		// that advertised models which failed the real request were filtered.
		validationCode = dominantModelValidationError(counts)
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
		transientModels:    transientModels,
		modelFormats:       modelFormats,
	}
}

// dedupeProbeModels removes exact model identifiers (after trimming) before
// any paid capability request is sent.  It deliberately does not collapse
// case, vendor prefixes, or version/date suffixes: those can be distinct
// callable models even when they look similar in a UI.
func dedupeProbeModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	unique := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		unique = append(unique, model)
	}
	return unique
}

func isFatalModelValidationCode(code string) bool {
	// 401/403 means the credential itself is unusable for every model. Other
	// categories are deliberately retained as per-model outcomes: a relay can
	// rate-limit or temporarily fail one model while serving the rest.
	return code == "auth"
}

func hasTransientValidationFailure(counts map[string]int) bool {
	if len(counts) == 0 {
		return false
	}
	for code, count := range counts {
		if count <= 0 {
			continue
		}
		switch code {
		case "rate_limited", "upstream", "network", "timeout":
			return true
		default:
			continue
		}
	}
	return false
}

func isTransientModelValidationCode(code string) bool {
	switch strings.TrimSpace(code) {
	case "rate_limited", "upstream", "network", "timeout":
		return true
	default:
		return false
	}
}

func classifyModelValidationError(ctx context.Context, status int, requestErr error) string {
	// Once the complete response body was read, a context deadline that fires
	// immediately afterwards must not rewrite that successful probe as a
	// timeout. The transport already reports an error when the body was not
	// completed; only then should cancellation/deadline precedence apply.
	if requestErr == nil && status >= 200 && status < 300 {
		return ""
	}
	if code := validationContextErrorCode(ctx); code != "" {
		// The caller's cancellation/deadline outranks a response that happened
		// to arrive at the same time (for example a 401 or 429).
		return code
	}
	if requestErr != nil {
		// A valid JSON error envelope means the model endpoint answered, but the
		// advertised model was rejected. Hide only that model and retain other
		// confirmed models.
		var envelope *upstreamErrorEnvelope
		if errors.As(requestErr, &envelope) {
			return classifySuccessfulErrorEnvelope(string(envelope.body))
		}
		var httpErr *upstreamHTTPError
		if errors.As(requestErr, &httpErr) {
			// A model-shaped message in an auth, rate-limit, or provider
			// response must not hide the stronger HTTP signal. Only validation
			// statuses that conventionally reject the selected model can be
			// classified as model_unavailable from their response text.
			modelStatus := httpErr.status == http.StatusBadRequest ||
				httpErr.status == http.StatusNotFound ||
				httpErr.status == http.StatusUnprocessableEntity
			if modelStatus && isModelUnavailableMessage(string(httpErr.body)) {
				return "model_unavailable"
			}
		}
	}
	// A generic 400/404/422 is still an HTTP/protocol failure. Only the
	// explicit model-specific message handled above may hide that model;
	// otherwise a malformed request or unsupported feature would incorrectly
	// remove a usable model from the verified catalogue.
	return classifyUpstreamTestError(ctx, status, requestErr)
}

func isModelScopedAuthFailure(err error) bool {
	var httpErr *upstreamHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.status != http.StatusUnauthorized && httpErr.status != http.StatusForbidden {
		return false
	}
	return isModelUnavailableMessage(string(httpErr.body))
}

// classifySuccessfulErrorEnvelope keeps common provider/application failures
// distinguishable even when a relay incorrectly returns HTTP 2xx with an
// OpenAI-style {"error": ...} body. It is still treated as a failed probe, but
// the operator sees the actionable category instead of a vague format error.
func classifySuccessfulErrorEnvelope(message string) string {
	lower := strings.ToLower(message)
	// Providers vary between human-readable messages and machine codes. Treat
	// underscores/hyphens as word separators so `invalid_api_key`,
	// `insufficient-quota`, and similar codes receive the same category as their
	// spaced equivalents.
	normalized := strings.NewReplacer("_", " ", "-", " ").Replace(lower)
	if isModelUnavailableMessage(lower) {
		return "model_unavailable"
	}
	for _, marker := range []string{"unauthorized", "forbidden", "authentication", "auth failed", "invalid api key", "invalid token", "api key"} {
		if strings.Contains(lower, marker) || strings.Contains(normalized, marker) {
			return "auth"
		}
	}
	for _, marker := range []string{"rate limit", "rate_limit", "too many requests", "quota exceeded", "quota limit", "throttl"} {
		if strings.Contains(lower, marker) || strings.Contains(normalized, marker) {
			return "rate_limited"
		}
	}
	for _, marker := range []string{"service unavailable", "provider unavailable", "temporarily unavailable", "bad gateway", "gateway timeout", "upstream error", "internal server error", "overloaded"} {
		if strings.Contains(lower, marker) || strings.Contains(normalized, marker) {
			return "upstream"
		}
	}
	return "invalid_response"
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
	normalized := strings.NewReplacer("_", " ", "-", " ").Replace(message)
	for _, marker := range []string{
		"model not found", "model_not_found", "unknown model", "invalid model",
		"model does not exist", "no such model", "model unavailable", "model not available", "model is not available",
		"model not enabled", "model is not enabled", "model disabled", "model access denied",
		"access denied for model", "no access to model", "does not have access to this model",
		"doesn't have access to this model", "permission denied for model",
		"model permission", "model forbidden", "model is not allowed", "model not allowed",
		"model restricted", "access restricted for model", "restricted model", "model not entitled",
		"not entitled to model", "model entitlement", "account is not entitled to use the model",
		"account not entitled to model", "project is not entitled to use the model",
		"model is not enabled for this project", "model is not enabled for this account",
	} {
		if strings.Contains(message, marker) || strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func parseUpstreamModels(body []byte) []string {
	models, _ := parseUpstreamModelsPayload(body)
	return models
}

// parseUpstreamModelsPayload returns the normalized model IDs and whether the
// response had a recognized catalogue shape. The second value matters for an
// empty, valid list: [] and {"data":[]} mean "no models", not malformed data.
func parseUpstreamModelsPayload(body []byte) ([]string, bool) {
	page, recognized := parseUpstreamModelsPagePayload(body)
	if !recognized || page.hasMore || page.nextURL != "" || page.nextCursor != "" {
		return nil, false
	}
	return page.models, true
}

// parseUpstreamModelsPagePayload accepts one model-list page and extracts a
// bounded continuation hint. It deliberately accepts the common nested
// `data.models`/`result.data` shapes used by relays while keeping arbitrary
// JSON objects out of the catalogue.
func parseUpstreamModelsPagePayload(body []byte) (advertisedModelsPage, bool) {
	var empty advertisedModelsPage
	body = trimUpstreamJSONBody(body)
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return empty, false
	}
	entries, meta, recognized := modelCatalogueEntries(root)
	if !recognized {
		return empty, false
	}
	if len(entries) > upstreamModelCatalogueMax {
		return empty, false
	}
	// The response body is already bounded by fetchAdvertisedModelsPath (1 MiB),
	// and this explicit cap keeps validation work bounded. Every advertised model
	// must either receive a real probe or leave the run incomplete; silently
	// truncating here would make a partial validation look complete.
	seen := make(map[string]struct{}, len(entries))
	models := make([]string, 0, len(entries))
	for _, entry := range entries {
		var name string
		switch value := entry.(type) {
		case string:
			name = value
		case map[string]any:
			// Prefer invocation identifiers over display labels. Relays often
			// return both `name` (a human title) and `slug`/`model_id` (the value
			// accepted by the completion route); selecting the label would make a
			// manually usable model look unavailable during the real probe.
			for _, key := range []string{"model_id", "slug", "model", "id", "model_name", "name"} {
				if candidate, ok := value[key].(string); ok {
					// Some providers include a preferred field with an empty
					// value and put the actual identifier in a later alias
					// (for example {"model_id":"", "id":"gpt-x"}).
					// Keep walking the priority list until a non-empty value is
					// found; an entirely empty entry is still rejected below.
					candidate = strings.TrimSpace(candidate)
					if candidate == "" {
						continue
					}
					name = candidate
					break
				}
			}
		}
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 200 {
			// A non-empty advertised list containing an invalid entry is a
			// malformed catalogue, not an empty catalogue. Rejecting the whole
			// response prevents validation from silently skipping a provider model.
			return empty, false
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		models = append(models, name)
	}
	return advertisedModelsPage{
		models:     models,
		recognized: true,
		hasMore:    meta.hasMore,
		nextURL:    meta.nextURL,
		nextCursor: meta.nextCursor,
		cursorKey:  meta.cursorKey,
	}, true
}

type modelCatalogueMeta struct {
	hasMore    bool
	nextURL    string
	nextCursor string
	cursorKey  string
}

const upstreamModelCatalogueMaxNesting = 3

// modelCatalogueEntries identifies the model array and continuation metadata
// without accepting a generic `status: ok` object as a catalogue. Relays can
// wrap the same list in more than one envelope (`payload.result.data`, for
// example), so the unwrapping is recursive but deliberately depth-limited.
func modelCatalogueEntries(root any) ([]any, modelCatalogueMeta, bool) {
	return modelCatalogueEntriesAtDepth(root, 0)
}

func modelCatalogueEntriesAtDepth(root any, depth int) ([]any, modelCatalogueMeta, bool) {
	if entries, ok := root.([]any); ok {
		return entries, modelCatalogueMeta{}, true
	}
	value, ok := root.(map[string]any)
	if !ok {
		return nil, modelCatalogueMeta{}, false
	}
	meta := modelCataloguePagination(value)
	for _, key := range []string{"data", "models", "items"} {
		if entries, ok := value[key].([]any); ok {
			return entries, meta, true
		}
	}
	if depth >= upstreamModelCatalogueMaxNesting {
		return nil, modelCatalogueMeta{}, false
	}
	// A few relays wrap the OpenAI list in `result`, `data`, or `response`
	// objects. Require a real array below the bounded wrapper chain so arbitrary
	// JSON status documents are not treated as model catalogues.
	for _, key := range []string{"result", "data", "response", "payload", "body"} {
		nested, ok := value[key].(map[string]any)
		if !ok {
			continue
		}
		entries, nestedMeta, recognized := modelCatalogueEntriesAtDepth(nested, depth+1)
		if recognized {
			return entries, mergeModelCatalogueMeta(meta, nestedMeta), true
		}
	}
	return nil, modelCatalogueMeta{}, false
}

func mergeModelCatalogueMeta(outer, nested modelCatalogueMeta) modelCatalogueMeta {
	if nested.hasMore || nested.nextURL != "" || nested.nextCursor != "" {
		return nested
	}
	return outer
}

func modelCataloguePagination(value map[string]any) modelCatalogueMeta {
	meta := modelCatalogueMeta{}
	for _, key := range []string{"has_more", "hasMore", "more"} {
		if hasMore, ok := value[key].(bool); ok {
			meta.hasMore = hasMore
			break
		}
	}
	for _, key := range []string{"next", "next_url", "next_uri", "next_page", "next_link"} {
		if next, ok := value[key].(string); ok && strings.TrimSpace(next) != "" {
			meta.nextURL = strings.TrimSpace(next)
			break
		}
	}
	for _, key := range []string{"next_cursor", "nextCursor", "next_page_token", "nextPageToken", "page_token"} {
		if cursor, ok := value[key].(string); ok && strings.TrimSpace(cursor) != "" {
			meta.nextCursor = strings.TrimSpace(cursor)
			switch strings.ToLower(key) {
			case "next_cursor", "nextcursor":
				meta.cursorKey = "cursor"
			case "next_page_token", "nextpagetoken", "page_token":
				meta.cursorKey = "page_token"
			}
			// `page_token` is also commonly used as the response's next-page
			// cursor (without a separate has_more flag). Treat every non-empty
			// cursor field as a continuation; the visited-URL guard prevents a
			// provider echoing the same token from creating an unbounded loop.
			meta.hasMore = true
			break
		}
	}
	if meta.hasMore && meta.nextCursor == "" {
		for _, key := range []string{"last_id", "lastId", "after"} {
			if cursor, ok := value[key].(string); ok && strings.TrimSpace(cursor) != "" {
				meta.nextCursor = strings.TrimSpace(cursor)
				meta.cursorKey = "after"
				break
			}
		}
	}
	return meta
}

func resolveAdvertisedModelsNextURL(current, next string) (string, bool) {
	currentURL, err := url.Parse(current)
	if err != nil || currentURL.Scheme == "" || currentURL.Host == "" {
		return "", false
	}
	candidate, err := url.Parse(strings.TrimSpace(next))
	if err != nil {
		return "", false
	}
	if !candidate.IsAbs() {
		candidate = currentURL.ResolveReference(candidate)
	}
	if candidate.Scheme != currentURL.Scheme || !strings.EqualFold(candidate.Host, currentURL.Host) {
		return "", false
	}
	candidate.Fragment = ""
	return candidate.String(), true
}

func appendAdvertisedModelsCursor(current, key, cursor string) (string, bool) {
	parsed, err := url.Parse(current)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	query := parsed.Query()
	query.Set(key, cursor)
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String(), true
}

func containsModel(models []string, target string) bool {
	for _, model := range models {
		if model == target {
			return true
		}
	}
	return false
}

type responsesProbeShape uint8

const (
	// Shapes are bit flags rather than mutually-exclusive enum values. A strict
	// relay may reject more than one field in sequence (for example it first
	// requires an input array and then requires stream=true); retaining earlier
	// fixes prevents a later retry from silently reverting them.
	responsesProbeCanonical responsesProbeShape = 0
	responsesProbeCompact   responsesProbeShape = 1 << iota
	responsesProbeInputArray
	responsesProbeStream
	responsesProbeNoOutputLimit
)

// sendUpstreamResponsesProbe starts with the ordinary Responses request and
// performs at most four compatibility retries.  A retry is allowed only when
// the relay explicitly rejected the request shape before model execution;
// quota, authentication, model, and provider errors are never replayed.
func sendUpstreamResponsesProbe(ctx context.Context, client *http.Client, target, key, model string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	status, requestErr := sendUpstreamTestRequest(ctx, client, target, key, model, false)
	shape := responsesProbeCanonical
	tried := map[responsesProbeShape]struct{}{shape: {}}
	// Parameter rejections are handled before model execution and therefore do
	// not consume model quota. Four bounded retries cover the independent
	// compatibility dimensions without allowing a malformed relay to hold the
	// validation worker indefinitely.
	for attempt := 0; attempt < 4 && ctx.Err() == nil; attempt++ {
		next := responsesProbeCompatibilityShape(status, requestErr)
		if next == responsesProbeCanonical {
			break
		}
		merged := shape | next
		if merged == shape {
			break
		}
		if _, exists := tried[merged]; exists {
			break
		}
		shape = merged
		tried[shape] = struct{}{}
		status, requestErr = sendUpstreamTestRequestShape(ctx, client, target, key, model, false, shape)
	}
	return status, requestErr
}

// sendUpstreamModelProbe selects the first working wire protocol.  Responses
// remains the preferred route, followed by Chat Completions and Anthropic
// Messages only when the preceding route explicitly rejects its protocol.
// Authentication mismatches are handled inside each request helper, while
// quota, model, and provider failures are never replayed through another
// protocol.
func sendUpstreamModelProbeWithFormat(ctx context.Context, client *http.Client, base, key, model string) (int, domain.RequestFormat, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	format := domain.FormatOpenAIResponses
	status, requestErr := sendUpstreamResponsesProbe(ctx, client, upstreamURL(base, "/v1/responses"), key, model)
	if shouldFallbackTestRequest(status, requestErr) && ctx.Err() == nil {
		format = domain.FormatOpenAIChat
		status, requestErr = sendUpstreamChatProbe(ctx, client, upstreamURL(base, "/v1/chat/completions"), key, model)
	}
	if shouldFallbackToMessagesRequest(status, requestErr, key) && ctx.Err() == nil {
		format = domain.FormatAnthropic
		status, requestErr = sendUpstreamMessagesProbe(ctx, client, upstreamURL(base, "/v1/messages"), key, model)
	}
	return status, format, requestErr
}

func sendUpstreamModelProbe(ctx context.Context, client *http.Client, base, key, model string) (int, error) {
	status, _, requestErr := sendUpstreamModelProbeWithFormat(ctx, client, base, key, model)
	return status, requestErr
}

// sendUpstreamModelProbeWithRetry retries only a transport failure that was
// observed before any request bytes were written. A response status (including
// 408/429/5xx) is never replayed because the upstream may already have charged
// or started the model request before returning that status. This keeps model
// validation conservative while a later validation run can recover a transient
// outage without risking duplicate spend in the same run.
func sendUpstreamModelProbeWithRetry(ctx context.Context, client *http.Client, base, key, model string) (int, error) {
	status, _, requestErr := sendUpstreamModelProbeWithRetryFormat(ctx, client, base, key, model)
	return status, requestErr
}

func sendUpstreamModelProbeWithRetryFormat(ctx context.Context, client *http.Client, base, key, model string) (int, domain.RequestFormat, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, _ := withUpstreamProbeAttemptState(ctx)
	status, format, requestErr := sendUpstreamModelProbeWithFormat(probeCtx, client, base, key, model)
	if !shouldRetryTransientModelProbe(status, requestErr) || !isPreSendProbeTransportError(requestErr) || !waitForUpstreamModelProbeRetry(ctx) {
		return status, format, requestErr
	}
	retryCtx, _ := withUpstreamProbeAttemptState(ctx)
	return sendUpstreamModelProbeWithFormat(retryCtx, client, base, key, model)
}

type upstreamProbeAttemptState struct {
	requestWritten atomic.Bool
}

type upstreamProbeAttemptStateKey struct{}

func withUpstreamProbeAttemptState(ctx context.Context) (context.Context, *upstreamProbeAttemptState) {
	if state, ok := ctx.Value(upstreamProbeAttemptStateKey{}).(*upstreamProbeAttemptState); ok && state != nil {
		return ctx, state
	}
	state := &upstreamProbeAttemptState{}
	return context.WithValue(ctx, upstreamProbeAttemptStateKey{}, state), state
}

type upstreamProbeTransportError struct {
	err            error
	requestWritten bool
	traceReliable  bool
}

func (e *upstreamProbeTransportError) Error() string {
	if e == nil || e.err == nil {
		return "upstream probe transport error"
	}
	return e.err.Error()
}

func (e *upstreamProbeTransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func isPreSendProbeTransportError(err error) bool {
	var transportErr *upstreamProbeTransportError
	return errors.As(err, &transportErr) && transportErr.traceReliable && !transportErr.requestWritten
}

type upstreamProbeTraceSupport interface {
	SupportsHTTPTrace() bool
}

func supportsUpstreamProbeTrace(client *http.Client) bool {
	if client == nil || client.Transport == nil {
		return client != nil
	}
	if _, ok := client.Transport.(*http.Transport); ok {
		return true
	}
	if support, ok := client.Transport.(upstreamProbeTraceSupport); ok {
		return support.SupportsHTTPTrace()
	}
	return false
}

func doUpstreamProbeRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	ctx, state := withUpstreamProbeAttemptState(req.Context())
	traceReliable := supportsUpstreamProbeTrace(client)
	trace := &httptrace.ClientTrace{
		// WroteHeaders is the earliest standard transport signal that any
		// request bytes may have left this process. WroteRequest also covers
		// transports which expose only the completed-write callback.
		WroteHeaders: func() { state.requestWritten.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) {
			state.requestWritten.Store(true)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))
	resp, err := client.Do(req)
	if err != nil {
		return resp, &upstreamProbeTransportError{
			err:            err,
			requestWritten: state.requestWritten.Load(),
			traceReliable:  traceReliable,
		}
	}
	return resp, nil
}

func shouldRetryTransientModelProbe(status int, requestErr error) bool {
	// Only a status-less transport error can be retried. Whether it happened
	// before the request was sent is checked separately by the caller.
	return status == 0 && requestErr != nil &&
		!errors.Is(requestErr, context.Canceled) &&
		!errors.Is(requestErr, context.DeadlineExceeded)
}

func waitForUpstreamModelProbeRetry(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	timer := time.NewTimer(upstreamModelProbeRetryDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func responsesProbeCompatibilityShape(status int, requestErr error) responsesProbeShape {
	if !shouldRetryResponsesParameter(status, requestErr) {
		return responsesProbeCanonical
	}
	body := strings.ToLower(responseErrorBody(requestErr))
	// Some relays implement the Responses wire format but require an input
	// message array instead of the shorthand string accepted by OpenAI.
	if strings.Contains(body, "input") && (strings.Contains(body, "array") ||
		strings.Contains(body, "list") || strings.Contains(body, "object") ||
		strings.Contains(body, "message") || strings.Contains(body, "expected")) {
		return responsesProbeInputArray
	}
	// A few gateways force streaming for Responses requests.  Their error
	// usually names both `stream` and the required true value.
	if strings.Contains(body, "stream") && (strings.Contains(body, "true") ||
		strings.Contains(body, "required") || strings.Contains(body, "must")) {
		return responsesProbeStream
	}
	// A subset of OpenAI-compatible relays implements Responses but rejects
	// max_output_tokens altogether (or only supports a provider-specific
	// default).  The tiny `hi` probe is bounded by its context, so dropping this
	// optional limit is preferable to hiding an otherwise usable model.
	if strings.Contains(body, "max_output_tokens") || strings.Contains(body, "max output tokens") {
		return responsesProbeNoOutputLimit
	}
	return responsesProbeCompact
}

// sendUpstreamChatProbe probes the legacy Chat Completions route and carries
// explicit request-shape fixes across retries. A number of relays require
// `max_completion_tokens` for newer models and some force `stream=true` even
// for a tiny capability request; treating those as protocol mismatches keeps
// a manually usable model from being filtered by the management probe.
func sendUpstreamChatProbe(ctx context.Context, client *http.Client, target, key, model string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	status, requestErr := sendUpstreamTestRequestShape(ctx, client, target, key, model, true, responsesProbeCanonical)
	shape := responsesProbeCanonical
	tried := map[responsesProbeShape]struct{}{shape: {}}
	// Chat has two independent compatibility dimensions. Keep the same bounded
	// retry discipline as Responses and never replay an ordinary model/provider
	// failure.
	for attempt := 0; attempt < 2 && ctx.Err() == nil; attempt++ {
		next := chatProbeCompatibilityShape(status, requestErr)
		if next == responsesProbeCanonical {
			break
		}
		merged := shape | next
		if merged == shape {
			break
		}
		if _, exists := tried[merged]; exists {
			break
		}
		shape = merged
		tried[shape] = struct{}{}
		status, requestErr = sendUpstreamTestRequestShape(ctx, client, target, key, model, true, shape)
	}
	return status, requestErr
}

func chatProbeCompatibilityShape(status int, requestErr error) responsesProbeShape {
	if !shouldRetryChatParameter(status, requestErr) {
		return responsesProbeCanonical
	}
	body := strings.ToLower(responseErrorBody(requestErr))
	if strings.Contains(body, "stream") && (strings.Contains(body, "true") ||
		strings.Contains(body, "required") || strings.Contains(body, "must")) {
		return responsesProbeStream
	}
	return responsesProbeCompact
}

// sendUpstreamMessagesProbe is the last compatibility route for relays that
// expose Anthropic's Messages API but do not implement either OpenAI route.
// The request is intentionally tiny and non-streaming.  Strict gateways may
// require stream=true or reject an optional field; those shape corrections are
// retried only when the response explicitly identifies the offending field.
func sendUpstreamMessagesProbe(ctx context.Context, client *http.Client, target, key, model string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	status, requestErr := sendUpstreamMessagesRequestShape(ctx, client, target, key, model, responsesProbeCanonical)
	shape := responsesProbeCanonical
	tried := map[responsesProbeShape]struct{}{shape: {}}
	for attempt := 0; attempt < 2 && ctx.Err() == nil; attempt++ {
		next := messagesProbeCompatibilityShape(status, requestErr)
		if next == responsesProbeCanonical {
			break
		}
		merged := shape | next
		if merged == shape {
			break
		}
		if _, exists := tried[merged]; exists {
			break
		}
		shape = merged
		tried[shape] = struct{}{}
		status, requestErr = sendUpstreamMessagesRequestShape(ctx, client, target, key, model, shape)
	}
	return status, requestErr
}

func messagesProbeCompatibilityShape(status int, requestErr error) responsesProbeShape {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return responsesProbeCanonical
	}
	body := strings.ToLower(responseErrorBody(requestErr))
	if !shouldRetryMessagesParameter(status, requestErr) {
		return responsesProbeCanonical
	}
	if isMessagesStreamRequiredMessage(body) {
		return responsesProbeStream
	}
	if isMessagesStreamRejectedMessage(body) {
		// `stream` is optional in the Messages contract. A relay that rejects
		// the field wants the same request with it omitted, not stream=true.
		return responsesProbeCompact
	}
	return responsesProbeCanonical
}

func isMessagesStreamRequiredMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if !strings.Contains(message, "stream") {
		return false
	}
	for _, marker := range []string{
		"stream must be true", "stream=true required", "stream=true is required",
		"stream parameter is required", "stream parameter required", "stream is required",
		"stream must be enabled", "stream must be enabled", "requires stream=true",
		"require stream=true", "use stream=true", "set stream=true",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func isMessagesStreamRejectedMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if !strings.Contains(message, "stream") {
		return false
	}
	for _, marker := range []string{
		"stream is not supported", "stream not supported", "stream is unsupported",
		"unsupported stream", "stream parameter is not supported", "stream parameter not supported",
		"stream is not allowed", "stream not allowed", "stream parameter is not allowed",
		"unknown parameter stream", "unknown field stream", "unrecognized field stream",
		"unrecognised field stream", "unexpected field stream", "extra field stream",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func shouldRetryMessagesParameter(status int, requestErr error) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		var envelope *upstreamErrorEnvelope
		if status < 200 || status >= 300 || !errors.As(requestErr, &envelope) {
			return false
		}
	}
	body := responseErrorBody(requestErr)
	// max_tokens is required by the Messages contract; retrying while changing
	// or removing it would only repeat a paid semantic failure.  Only optional
	// stream/messages shape complaints are eligible here.
	return isExplicitParameterCompatibilityMessage(body, "stream", "messages")
}

// sendUpstreamMessagesRequest sends one Anthropic-compatible probe.  The
// canonical header is x-api-key; a Bearer retry is allowed only after a 401 or
// 403 that looks like an authentication/header mismatch.  This lets generic
// OpenAI relays that happen to expose /v1/messages work without making the
// operator choose a protocol-specific credential setting.
func sendUpstreamMessagesRequestShape(ctx context.Context, client *http.Client, target, key, model string, shape responsesProbeShape) (int, error) {
	payload := map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"stream":     false,
	}
	if shape&responsesProbeStream != 0 {
		payload["stream"] = true
	}
	if shape&responsesProbeCompact != 0 {
		// A few Anthropic-compatible relays reject the optional stream field
		// unless streaming was explicitly requested.  The compact retry removes
		// it while retaining the required max_tokens/messages fields.
		delete(payload, "stream")
	}
	body, _ := json.Marshal(payload)
	lastStatus := 0
	var lastErr error
	for index, mode := range upstreamAuthModes(key, upstreamAuthAPIKey) {
		status, requestErr := sendUpstreamMessagesRequestOnce(ctx, client, target, key, model, body, mode)
		lastStatus, lastErr = status, requestErr
		if status == 0 || index == 1 || !shouldRetryUpstreamAuthMode(status, requestErr, mode) {
			break
		}
	}
	return lastStatus, lastErr
}

func sendUpstreamMessagesRequestOnce(ctx context.Context, client *http.Client, target, key, model string, body []byte, mode upstreamAuthMode) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	setUpstreamAuthHeader(req, key, mode)
	// Anthropic-compatible relays generally require this version header, while
	// OpenAI-compatible relays safely ignore it.  Keep it on both auth attempts.
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := doUpstreamProbeRequest(client, req)
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, errors.New("nil upstream response")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		responseBody, readErr := readUpstreamProbeBody(resp)
		if readErr != nil {
			return resp.StatusCode, readErr
		}
		if len(responseBody) == 0 || len(responseBody) > 1<<20 || !isUpstreamSuccessResponse(responseBody) {
			if isUpstreamFailureResponse(responseBody) {
				return resp.StatusCode, &upstreamErrorEnvelope{body: append([]byte(nil), responseBody...)}
			}
			return resp.StatusCode, errInvalidUpstreamResponse
		}
		return resp.StatusCode, nil
	}
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, &upstreamHTTPError{status: resp.StatusCode, body: responseBody}
}

func sendUpstreamTestRequest(ctx context.Context, client *http.Client, target, key, model string, chat bool) (int, error) {
	return sendUpstreamTestRequestVariant(ctx, client, target, key, model, chat, false)
}

// sendUpstreamTestRequestVariant sends the normal minimal probe or one
// compatibility variant. The default shape remains stable for existing
// clients; the variant omits optional fields that strict relays reject before
// dispatching a request.
func sendUpstreamTestRequestVariant(ctx context.Context, client *http.Client, target, key, model string, chat, compact bool) (int, error) {
	if !chat {
		shape := responsesProbeCanonical
		if compact {
			shape = responsesProbeCompact
		}
		return sendUpstreamTestRequestShape(ctx, client, target, key, model, false, shape)
	}
	shape := responsesProbeCanonical
	if compact {
		shape = responsesProbeCompact
	}
	return sendUpstreamTestRequestShape(ctx, client, target, key, model, true, shape)
}

func sendUpstreamTestRequestShape(ctx context.Context, client *http.Client, target, key, model string, chat bool, shape responsesProbeShape) (int, error) {
	key = normalizeUpstreamKey(key)
	var body []byte
	if chat {
		payload := map[string]any{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
			"stream":   false,
		}
		if shape&responsesProbeStream != 0 {
			payload["stream"] = true
		}
		if shape&responsesProbeCompact != 0 {
			// Newer reasoning models reject the legacy max_tokens field. Keep
			// the compatibility retry limited to an explicit parameter error.
			payload["max_completion_tokens"] = 1
		} else {
			payload["max_tokens"] = 1
		}
		body, _ = json.Marshal(payload)
	} else {
		payload := map[string]any{
			"model":             model,
			"input":             "hi",
			"stream":            false,
			"max_output_tokens": 1,
		}
		if shape&responsesProbeInputArray != 0 {
			payload["input"] = []map[string]any{{"role": "user", "content": "hi"}}
		}
		if shape&responsesProbeStream != 0 {
			payload["stream"] = true
		}
		if shape&responsesProbeNoOutputLimit != 0 {
			delete(payload, "max_output_tokens")
		}
		if shape&responsesProbeCompact == 0 && shape&responsesProbeInputArray == 0 && shape&responsesProbeStream == 0 && shape&responsesProbeNoOutputLimit == 0 {
			// `store` is supported by the Responses API but a number of
			// OpenAI-compatible relays reject unknown optional fields. It is
			// kept in the normal request for wire compatibility and omitted by
			// the one-shot parameter retry below.
			payload["store"] = false
		}
		body, _ = json.Marshal(payload)
	}
	lastStatus := 0
	var lastErr error
	for index, mode := range upstreamAuthModes(key, upstreamAuthBearer) {
		status, requestErr := sendUpstreamTestRequestShapeOnce(ctx, client, target, key, body, chat, mode)
		lastStatus, lastErr = status, requestErr
		if status == 0 || index == 1 || !shouldRetryUpstreamAuthMode(status, requestErr, mode) {
			break
		}
	}
	return lastStatus, lastErr
}

func sendUpstreamTestRequestShapeOnce(ctx context.Context, client *http.Client, target, key string, body []byte, chat bool, mode upstreamAuthMode) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	setUpstreamAuthHeader(req, key, mode)
	resp, err := doUpstreamProbeRequest(client, req)
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
		body, readErr := readUpstreamProbeBody(resp)
		if readErr != nil {
			return resp.StatusCode, readErr
		}
		if len(body) == 0 || len(body) > 1<<20 || !isUpstreamSuccessResponse(body) {
			if isUpstreamFailureResponse(body) {
				return resp.StatusCode, &upstreamErrorEnvelope{body: append([]byte(nil), body...)}
			}
			return resp.StatusCode, errInvalidUpstreamResponse
		}
		return resp.StatusCode, nil
	}
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, &upstreamHTTPError{status: resp.StatusCode, body: responseBody}
}

// readUpstreamProbeBody reads a bounded probe response. Streaming relays may
// keep the HTTP connection open after emitting the first complete SSE event;
// once that event proves a successful provider response, return immediately so
// validation does not turn an otherwise usable model into a timeout.
func readUpstreamProbeBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, errors.New("nil upstream response body")
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	reader := bufio.NewReaderSize(io.LimitReader(resp.Body, 1<<20+1), 32<<10)
	if resp.ContentLength > 1<<20 {
		return nil, errors.New("upstream probe response too large")
	}
	// A normal JSON response with a declared length has a complete boundary
	// even when the server keeps the TCP connection alive. Read exactly that
	// bounded body instead of waiting for a newline or EOF.
	if !strings.Contains(contentType, "text/event-stream") && resp.ContentLength >= 0 && resp.ContentLength <= 1<<20 {
		body, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		return body, nil
	}
	// Chunked JSON relays may omit both a newline and a terminating chunk while
	// retaining the connection for reuse. Only take the JSON decoder fast path
	// when the first byte is actually a JSON object/array. Some proxies rewrite
	// SSE to application/json; handing its `data:`/`event:` prefix to Decoder
	// would consume the prefix and wait until the request deadline.
	if !strings.Contains(contentType, "text/event-stream") && isJSONContentType(contentType) {
		if jsonStart, peekErr := consumeUpstreamJSONPreamble(reader); peekErr == nil && jsonStart {
			var value json.RawMessage
			if err := json.NewDecoder(reader).Decode(&value); err == nil && len(value) > 0 {
				return value, nil
			} else if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
		}
	}
	// A proxy may rewrite `text/event-stream` to `application/json`. Peek at
	// the first complete line so an open stream is recognized by its SSE field
	// syntax instead of waiting for EOF like an ordinary JSON response. A few
	// relays prepend a blank line (or an SSE `id`/`retry` field), so retain a
	// small prefix while looking past only those unambiguous SSE preamble lines.
	firstLine, firstErr := reader.ReadBytes('\n')
	prefix := append([]byte(nil), firstLine...)
	looksLikeSSE := strings.Contains(contentType, "text/event-stream") || isUpstreamSSELine(firstLine)
	if !looksLikeSSE {
		// Leading empty lines are legal SSE separators but are also harmless in
		// an ordinary response. Only read ahead while the prefix is blank; this
		// keeps JSON responses from acquiring an extra blocking read while still
		// recognizing `\n\ndata: ...` streams when the content type was rewritten.
		for i := 0; i < 16 && isUpstreamSSEBlankLine(firstLine) && firstErr == nil; i++ {
			next, err := reader.ReadBytes('\n')
			prefix = append(prefix, next...)
			firstLine, firstErr = next, err
			if isUpstreamSSELine(next) {
				looksLikeSSE = true
				break
			}
		}
	}
	if !looksLikeSSE {
		// Chunked JSON relays sometimes flush a complete one-line response and
		// keep the connection alive for reuse without sending an end marker.
		// The provider envelope is already sufficient evidence for a capability
		// probe, so stop at that boundary instead of waiting for EOF. Multi-line
		// or incomplete JSON continues through the bounded full read below.
		if isUpstreamFailureResponse(prefix) {
			return prefix, &upstreamErrorEnvelope{body: append([]byte(nil), prefix...)}
		}
		if isUpstreamSuccessResponse(prefix) {
			return prefix, nil
		}
		if firstErr != nil && !errors.Is(firstErr, io.EOF) {
			return nil, firstErr
		}
		rest, readErr := io.ReadAll(reader)
		body := append(prefix, rest...)
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		return body, readErr
	}
	requestCtx := context.Background()
	if resp.Request != nil && resp.Request.Context() != nil {
		requestCtx = resp.Request.Context()
	}
	return readUpstreamProbeSSEBody(reader, resp.Body, prefix, firstErr, requestCtx)
}

// readUpstreamProbeSSEBody consumes an SSE-shaped probe response while keeping
// a short settlement window after the first successful provider frame. A
// number of relays emit response.*.delta (or a role-only chat chunk) and only
// then send response.failed; returning at the first delta would publish a
// broken model as available. The bounded window catches those follow-up
// failures without waiting for relays that leave keep-alive streams open.
func readUpstreamProbeSSEBody(reader *bufio.Reader, body io.Closer, prefix []byte, firstErr error, ctx context.Context) ([]byte, error) {
	if reader == nil {
		return nil, errors.New("nil upstream probe reader")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	all := make([]byte, 0, len(prefix))
	event := bytes.Buffer{}
	settled := false
	var settlementTimer *time.Timer
	var settlementC <-chan time.Time
	armSettlement := func() {
		if settlementTimer == nil {
			settlementTimer = time.NewTimer(upstreamProbeSSESettlementWindow)
		} else {
			if !settlementTimer.Stop() {
				select {
				case <-settlementTimer.C:
				default:
				}
			}
			settlementTimer.Reset(upstreamProbeSSESettlementWindow)
		}
		settlementC = settlementTimer.C
	}
	stopSettlement := func() {
		if settlementTimer != nil {
			if !settlementTimer.Stop() {
				select {
				case <-settlementTimer.C:
				default:
				}
			}
		}
	}
	defer stopSettlement()

	// Process one physical SSE line. We inspect after data lines as well as at
	// blank event boundaries because some relays omit the final blank separator
	// when they keep the connection alive. The failure check always precedes the
	// success check so a frame containing both signals cannot be accepted.
	processLine := func(line []byte) (failure, success bool, err error) {
		if len(all)+len(line) > 1<<20 {
			return false, false, errors.New("upstream probe response too large")
		}
		all = append(all, line...)
		event.Write(line)
		if event.Len() > 1<<20 {
			return false, false, errors.New("upstream probe response too large")
		}
		trimmed := trimUpstreamSSELine(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			if isUpstreamFailureResponse(event.Bytes()) {
				return true, false, nil
			}
			if isUpstreamSuccessResponse(event.Bytes()) {
				return false, true, nil
			}
		}
		if isUpstreamSSEBlankLine(line) {
			if isUpstreamFailureResponse(event.Bytes()) {
				return true, false, nil
			}
			if isUpstreamSuccessResponse(event.Bytes()) {
				return false, true, nil
			}
			event.Reset()
		}
		return false, false, nil
	}

	// The prefix can contain blank preamble lines plus the first data frame.
	// Replay it through the same event parser rather than treating it as one
	// opaque event; this preserves event names and multiline data semantics.
	processPrefix := func() (bool, error) {
		remaining := prefix
		for len(remaining) > 0 {
			line := remaining
			if index := bytes.IndexByte(remaining, '\n'); index >= 0 {
				line = remaining[:index+1]
				remaining = remaining[index+1:]
			} else {
				remaining = nil
			}
			failure, success, err := processLine(line)
			if err != nil {
				return false, err
			}
			if failure {
				return false, nil
			}
			if success {
				settled = true
				armSettlement()
			}
		}
		return true, nil
	}
	if _, err := processPrefix(); err != nil {
		return nil, err
	}
	// A failure may arrive in the buffered prefix before any success frame. Do
	// not discard it just because the settlement window was never armed.
	if isUpstreamFailureResponse(all) {
		return all, &upstreamErrorEnvelope{body: append([]byte(nil), all...)}
	}
	if firstErr != nil {
		if errors.Is(firstErr, io.EOF) {
			if settled {
				return append([]byte(nil), all...), nil
			}
			return nil, firstErr
		}
		if settled {
			return append([]byte(nil), all...), firstErr
		}
		return nil, firstErr
	}

	type lineResult struct {
		line []byte
		err  error
	}
	lines := make(chan lineResult, 1)
	stopReader := make(chan struct{})
	go func() {
		defer close(lines)
		for {
			line, err := reader.ReadBytes('\n')
			result := lineResult{line: line, err: err}
			select {
			case lines <- result:
			case <-stopReader:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		close(stopReader)
		// The caller also closes response bodies, but closing here is required
		// when the settlement timer fires while the reader goroutine is blocked
		// on an open keep-alive connection.
		if body != nil {
			_ = body.Close()
		}
	}()

	for {
		select {
		case <-settlementC:
			if settled {
				return append([]byte(nil), all...), nil
			}
			settlementC = nil
		case <-ctx.Done():
			// A success delta is provisional until the bounded settlement window
			// completes. If the request context expires first, report the
			// cancellation instead of publishing a false-positive capability.
			return nil, ctx.Err()
		case result, ok := <-lines:
			if !ok {
				if settled {
					return append([]byte(nil), all...), nil
				}
				return nil, io.EOF
			}
			if len(result.line) > 0 {
				failure, success, err := processLine(result.line)
				if err != nil {
					return nil, err
				}
				if failure {
					return append([]byte(nil), all...), &upstreamErrorEnvelope{body: append([]byte(nil), all...)}
				}
				if success {
					settled = true
					armSettlement()
				}
			}
			if result.err != nil {
				if errors.Is(result.err, io.EOF) {
					if settled {
						return append([]byte(nil), all...), nil
					}
				}
				return nil, result.err
			}
		}
	}
}

// consumeUpstreamJSONPreamble removes only insignificant leading whitespace
// and an optional UTF-8 BOM, then reports whether the next byte begins a JSON
// object or array. It deliberately leaves non-JSON bytes untouched so a proxy
// that mislabeled SSE as application/json can still be parsed below.
func consumeUpstreamJSONPreamble(reader *bufio.Reader) (bool, error) {
	for consumed := 0; consumed < 64; consumed++ {
		first, err := reader.Peek(1)
		if err != nil {
			return false, err
		}
		switch first[0] {
		case '{', '[':
			return true, nil
		case ' ', '\t', '\r', '\n':
			_, _ = reader.ReadByte()
			continue
		case 0xef:
			bom, bomErr := reader.Peek(3)
			if bomErr == nil && bytes.Equal(bom, []byte{0xef, 0xbb, 0xbf}) {
				_, _ = reader.Discard(3)
				consumed += 2
				continue
			}
			return false, nil
		default:
			return false, nil
		}
	}
	return false, nil
}

// isJSONContentType recognizes JSON media types without treating a vendor
// suffix or parameters as a different protocol.  Relays commonly return
// `application/vnd.provider+json; charset=utf-8`; using MIME parsing keeps
// quoted parameters and casing from taking the fast path away accidentally.
func isJSONContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		// A malformed parameter must not make us classify an otherwise obvious
		// JSON type as plain text. Keep the fallback deliberately conservative.
		mediaType = strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == "application/json" || mediaType == "text/json" || strings.HasSuffix(mediaType, "+json")
}

func isUpstreamSSELine(line []byte) bool {
	line = trimUpstreamSSELine(line)
	return bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:")) ||
		bytes.HasPrefix(line, []byte("id:")) || bytes.HasPrefix(line, []byte("retry:")) ||
		bytes.HasPrefix(line, []byte(":"))
}

func isUpstreamSSEBlankLine(line []byte) bool {
	return len(trimUpstreamSSELine(line)) == 0
}

func trimUpstreamSSELine(line []byte) []byte {
	line = bytes.TrimSpace(line)
	// UTF-8 BOMs are occasionally emitted by a proxy before the first SSE
	// field. Strip one after whitespace trimming so `\ufeffdata:` is treated
	// exactly like the standards-compliant `data:` spelling.
	line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
	return bytes.TrimSpace(line)
}

var errInvalidUpstreamResponse = errors.New("invalid upstream test response")
var errUpstreamErrorEnvelope = errors.New("upstream returned an error envelope")

// trimUpstreamJSONBody removes the optional UTF-8 BOM and surrounding
// whitespace emitted by a few relays or intermediary proxies. The standard
// encoding/json package rejects a BOM even though it is valid in many real
// world JSON feeds, so every JSON classification path uses this helper.
func trimUpstreamJSONBody(body []byte) []byte {
	body = bytes.TrimSpace(body)
	body = bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})
	return bytes.TrimSpace(body)
}

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
	body = trimUpstreamJSONBody(body)
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return false
	}
	return isJSONObjectResponseMap(value, 0)
}

func isJSONObjectResponseMap(value map[string]any, depth int) bool {
	if value == nil {
		return false
	}
	if depth >= 8 {
		return false
	}
	// Some relays incorrectly return HTTP 200 for an application-level error.
	// Treating that envelope as a successful probe would mark a broken upstream
	// healthy and make the scheduler route real traffic into it. A top-level
	// error member is the common OpenAI-compatible shape; nested payload fields
	// remain provider-specific and are intentionally not rejected here.
	if rawError, hasError := value["error"]; hasError && rawError != nil {
		return false
	}
	if hasExplicitUpstreamFailure(value) || isNeutralUpstreamEvent(value) {
		return false
	}
	// A generic 200 JSON document (for example a portal's {"status":"ok"})
	// does not prove that the selected model endpoint answered. Require a
	// recognized response envelope used by the supported APIs instead of merely
	// accepting any arbitrary JSON object.
	if isChunk, meaningful := classifyChatCompletionChunk(value); isChunk {
		return meaningful
	}
	if object, ok := value["object"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(object)) {
		case "chat.completion", "completion", "text_completion", "response", "message", "image", "image_generation":
			return true
		}
	}
	if responseType, ok := value["type"].(string); ok {
		responseType = strings.ToLower(strings.TrimSpace(responseType))
		switch responseType {
		case "response", "message", "chat.completion", "completion", "image", "image_generation":
			return true
		}
		// Responses streaming events use names such as
		// `response.completed` and `response.output_text.delta`. The
		// presence of a non-error event proves the endpoint answered even
		// when the relay omits the nested `response.id`.
		if strings.HasPrefix(responseType, "response.") || strings.HasPrefix(responseType, "message.") ||
			strings.HasPrefix(responseType, "message_") || strings.HasPrefix(responseType, "content_block_") {
			return true
		}
	}
	if status, ok := value["status"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "completed", "incomplete", "succeeded", "success":
			if id, idOK := value["id"].(string); idOK && strings.TrimSpace(id) != "" {
				return true
			}
		}
	}
	if _, ok := value["choices"].([]any); ok {
		// The envelope itself proves that the completion route answered. A
		// filtered, tool-only, or otherwise empty completion can legitimately
		// contain no choices; requiring one item made a usable model look like
		// an invalid response during management validation.
		return true
	}
	if _, ok := value["output"].([]any); ok {
		// Responses providers may complete with an empty output array while
		// still returning a valid response envelope.
		return true
	}
	if outputText, ok := value["output_text"].(string); ok && strings.TrimSpace(outputText) != "" {
		return true
	}
	for _, key := range []string{"response", "message", "data", "result", "payload", "body"} {
		switch item := value[key].(type) {
		case map[string]any:
			if isJSONObjectResponseMap(item, depth+1) {
				return true
			}
		case []any:
			for _, entry := range item {
				child, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				if isJSONObjectResponseMap(child, depth+1) || isUpstreamImageResult(child) {
					return true
				}
			}
		}
	}
	return false
}

// classifyChatCompletionChunk distinguishes a streaming preamble from proof
// that the selected model produced a result. OpenAI-compatible streams commonly
// emit a role-only or empty delta first; accepting that frame can hide a failure
// in the following event and publish an unusable model as healthy.
func classifyChatCompletionChunk(value map[string]any) (bool, bool) {
	object, _ := value["object"].(string)
	isChunk := strings.EqualFold(strings.TrimSpace(object), "chat.completion.chunk")
	choices, hasChoices := value["choices"].([]any)
	if !hasChoices {
		return isChunk, false
	}
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		delta, hasDelta := choice["delta"].(map[string]any)
		if _, present := choice["delta"]; present {
			isChunk = true
		}
		if hasDelta {
			if hasMeaningfulChatChunkValue(delta["content"]) ||
				hasMeaningfulChatChunkValue(delta["tool_calls"]) ||
				hasMeaningfulChatChunkValue(delta["function_call"]) {
				return true, true
			}
		}
		if finishReason, ok := choice["finish_reason"].(string); ok && strings.TrimSpace(finishReason) != "" {
			return true, true
		}
	}
	return isChunk, false
}

func hasMeaningfulChatChunkValue(value any) bool {
	switch item := value.(type) {
	case string:
		return strings.TrimSpace(item) != ""
	case []any:
		return len(item) > 0
	case map[string]any:
		return len(item) > 0
	default:
		return false
	}
}

func isUpstreamImageResult(value map[string]any) bool {
	if value == nil {
		return false
	}
	for _, key := range []string{"url", "b64_json", "revised_prompt"} {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return true
		}
	}
	return false
}

func hasExplicitUpstreamFailure(value map[string]any) bool {
	return hasExplicitUpstreamFailureAt(value, 0)
}

func hasExplicitUpstreamFailureAt(value map[string]any, depth int) bool {
	if value == nil {
		return false
	}
	if depth >= 8 {
		return false
	}
	if rawError, ok := value["error"]; ok && rawError != nil {
		return true
	}
	for _, key := range []string{"success", "ok"} {
		if flag, ok := value[key].(bool); ok && !flag {
			return true
		}
	}
	if status, ok := value["status"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "error", "failed", "failure", "cancelled", "canceled", "denied", "unauthorized":
			return true
		}
	}
	for _, key := range []string{"type", "object"} {
		kind, ok := value[key].(string)
		if !ok {
			continue
		}
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind == "error" || kind == "message_error" || strings.HasSuffix(kind, ".error") || strings.HasSuffix(kind, ".failed") || strings.HasSuffix(kind, ".cancelled") || strings.HasSuffix(kind, ".canceled") {
			return true
		}
	}
	for _, key := range []string{"response", "message", "data", "result", "payload", "body"} {
		switch nested := value[key].(type) {
		case map[string]any:
			if hasExplicitUpstreamFailureAt(nested, depth+1) {
				return true
			}
		case []any:
			for _, item := range nested {
				if child, ok := item.(map[string]any); ok && hasExplicitUpstreamFailureAt(child, depth+1) {
					return true
				}
			}
		}
	}
	return false
}

func isNeutralUpstreamEvent(value map[string]any) bool {
	return isNeutralUpstreamEventAt(value, 0)
}

func isNeutralUpstreamEventAt(value map[string]any, depth int) bool {
	if value == nil {
		return false
	}
	if depth >= 8 {
		return false
	}
	if status, ok := value["status"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "queued", "pending", "processing", "in_progress":
			return true
		}
	}
	responseType, _ := value["type"].(string)
	switch strings.ToLower(strings.TrimSpace(responseType)) {
	case "ping", "response.created", "response.queued", "response.in_progress", "message_start", "content_block_start":
		return true
	}
	for _, key := range []string{"response", "message", "data", "result", "payload", "body"} {
		switch nested := value[key].(type) {
		case map[string]any:
			if isNeutralUpstreamEventAt(nested, depth+1) {
				return true
			}
		case []any:
			for _, item := range nested {
				if child, ok := item.(map[string]any); ok && isNeutralUpstreamEventAt(child, depth+1) {
					return true
				}
			}
		}
	}
	return false
}

// isUpstreamSuccessResponse accepts both ordinary JSON responses and relays
// that emit one or more SSE `data:` frames despite receiving stream=false.
// We only accept a frame whose JSON payload has a recognized provider shape;
// HTML/login pages and bare keep-alives remain failures.
func isUpstreamSuccessResponse(body []byte) bool {
	if isJSONObjectResponse(body) {
		return true
	}
	return visitUpstreamSSEPayloads(body, func(eventName string, payload []byte) bool {
		return !isFailureUpstreamSSEEvent(eventName) && !isNeutralUpstreamSSEEvent(eventName) && isJSONObjectResponse(payload)
	})
}

func isUpstreamFailureResponse(body []byte) bool {
	body = trimUpstreamJSONBody(body)
	var value map[string]any
	if json.Unmarshal(body, &value) == nil && hasExplicitUpstreamFailure(value) {
		return true
	}
	return visitUpstreamSSEPayloads(body, func(eventName string, payload []byte) bool {
		if isFailureUpstreamSSEEvent(eventName) {
			return true
		}
		var event map[string]any
		return json.Unmarshal(trimUpstreamJSONBody(payload), &event) == nil && hasExplicitUpstreamFailure(event)
	})
}

func isFailureUpstreamSSEEvent(eventName string) bool {
	eventName = strings.ToLower(strings.TrimSpace(eventName))
	return eventName == "error" || eventName == "message_error" || strings.HasSuffix(eventName, ".error") || strings.HasSuffix(eventName, ".failed") || strings.HasSuffix(eventName, ".cancelled") || strings.HasSuffix(eventName, ".canceled")
}

func isNeutralUpstreamSSEEvent(eventName string) bool {
	switch strings.ToLower(strings.TrimSpace(eventName)) {
	case "ping", "response.created", "response.queued", "response.in_progress", "message_start", "content_block_start":
		return true
	default:
		return false
	}
}

func visitUpstreamSSEPayloads(body []byte, visit func(string, []byte) bool) bool {
	// SSE permits an event payload to span multiple `data:` lines. Join the
	// physical lines with the protocol-mandated newline and inspect each event
	// at its blank-line boundary. Parsing each line independently would reject
	// valid long JSON frames and hide a manually usable model.
	var event bytes.Buffer
	eventName := ""
	flush := func() bool {
		if event.Len() == 0 {
			eventName = ""
			return false
		}
		payload := event.Bytes()
		if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			event.Reset()
			eventName = ""
			return false
		}
		ok := visit(eventName, payload)
		event.Reset()
		eventName = ""
		return ok
	}
	for _, rawLine := range bytes.Split(body, []byte{'\n'}) {
		trimmed := trimUpstreamSSELine(rawLine)
		if len(trimmed) == 0 {
			if flush() {
				return true
			}
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			eventName = strings.TrimSpace(string(bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("event:")))))
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if len(payload) == 0 {
			continue
		}
		if event.Len() > 0 {
			event.WriteByte('\n')
		}
		event.Write(payload)
	}
	return flush()
}

func responseErrorBody(err error) string {
	var httpErr *upstreamHTTPError
	if errors.As(err, &httpErr) {
		return string(httpErr.body)
	}
	// Some relays incorrectly return an OpenAI-style error envelope with HTTP
	// 2xx when they reject an optional request field. Expose that bounded body
	// to the same narrow compatibility classifier used for HTTP errors; without
	// this, a valid model can be reported as an invalid response before the
	// compact request variant gets a chance.
	var envelope *upstreamErrorEnvelope
	if errors.As(err, &envelope) {
		return string(envelope.body)
	}
	return ""
}

// isExplicitParameterCompatibilityMessage recognizes validation errors that
// identify an unsupported optional request field. These errors are generated
// before model execution, so one compact retry does not duplicate a paid
// request. Provider/model errors without an explicit field marker do not
// trigger this path.
func isExplicitParameterCompatibilityMessage(message string, fields ...string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	parameterMarkers := []string{
		"unknown field", "unknown parameter", "unsupported parameter", "unsupported field",
		"unrecognized field", "unrecognised field", "unexpected field", "additional properties",
		"not allowed", "not supported", "unsupported", "extra inputs are not permitted", "extra fields not permitted",
		"invalid parameter", "invalid field", "does not permit", "must be", "expected type",
		"expected an", "expects", "requires",
	}
	matchedMarker := false
	for _, marker := range parameterMarkers {
		if strings.Contains(lower, marker) {
			matchedMarker = true
			break
		}
	}
	if !matchedMarker {
		return false
	}
	for _, field := range fields {
		field = strings.ToLower(strings.TrimSpace(field))
		if field != "" && strings.Contains(lower, field) {
			return true
		}
	}
	return false
}

func shouldRetryResponsesParameter(status int, requestErr error) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		// A small number of relays wrap request validation failures in a 2xx
		// error envelope. Only that typed envelope is eligible; a generic 2xx
		// invalid response must never cause a second paid probe.
		var envelope *upstreamErrorEnvelope
		if status < 200 || status >= 300 || !errors.As(requestErr, &envelope) {
			return false
		}
	}
	body := responseErrorBody(requestErr)
	return isExplicitParameterCompatibilityMessage(body, "store", "max_output_tokens", "stream", "input")
}

func shouldRetryChatParameter(status int, requestErr error) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		var envelope *upstreamErrorEnvelope
		if status < 200 || status >= 300 || !errors.As(requestErr, &envelope) {
			return false
		}
	}
	body := responseErrorBody(requestErr)
	return isExplicitParameterCompatibilityMessage(body, "max_tokens", "max_completion_tokens", "stream")
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
		if !errors.As(requestErr, &envelope) {
			return false
		}
		// A 2xx error envelope means the relay accepted and processed the
		// request, then returned an application-level failure. Retrying the same
		// prompt through Chat Completions is not a safe protocol probe: it can
		// charge the account twice for quota/auth/provider failures. Only an
		// explicit Responses capability rejection justifies the compatibility
		// retry. Model-specific errors are intentionally excluded as well.
		return isExplicitProtocolFallbackMessage(string(envelope.body))
	}
	// A 2xx body that is not a recognized API response (for example a proxy
	// login page) is not evidence that the Responses route is unsupported.
	// Retrying it through Chat Completions would send a second paid request and
	// normally produce the same invalid body. Only an explicit protocol error
	// below may authorize a compatibility retry.
	if errors.Is(requestErr, errInvalidUpstreamResponse) {
		return false
	}
	// A few Chat-only relays answer the Responses route with a gateway-style
	// 500 (or another route status) instead of the usual 404/405. The body is
	// still a safe capability signal when it explicitly names an unsupported
	// route; provider, quota, and authentication errors remain excluded by the
	// narrow classifier below. Some relays use 403 for the unsupported route,
	// so allow that status only when the body explicitly says Chat is required.
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return isExplicitProtocolFallbackMessage(responseErrorBody(requestErr))
	}
	if status >= 400 {
		if body := responseErrorBody(requestErr); body != "" && isExplicitProtocolFallbackMessage(body) {
			return true
		}
	}
	if status == http.StatusNotFound {
		var httpErr *upstreamHTTPError
		if !errors.As(requestErr, &httpErr) || len(strings.TrimSpace(string(httpErr.body))) == 0 {
			// A bodyless 404 is the conventional signal for a missing route,
			// so trying the legacy endpoint is still useful.
			return true
		}
		message := string(httpErr.body)
		if isStructuredProviderFailure([]byte(message)) {
			return false
		}
		// Plain-text/HTML 404 pages are normally generated by a router before
		// any model work occurs, so they are safe route-compatibility signals.
		// Keep structured JSON errors conservative because they may describe a
		// provider/model rejection instead of a missing endpoint.
		var structured map[string]any
		if json.Unmarshal(trimUpstreamJSONBody([]byte(message)), &structured) != nil {
			return true
		}
		// A model-specific 404 was excluded above. At this point a generic JSON
		// 404 is the common router response emitted by Chat-only relays for the
		// unsupported Responses path. Retrying Chat Completions is necessary to
		// avoid hiding a genuinely usable model from the verified catalogue.
		return true
	}
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return shouldFallbackTest(status)
	}
	var httpErr *upstreamHTTPError
	if !errors.As(requestErr, &httpErr) {
		return false
	}
	if len(strings.TrimSpace(string(httpErr.body))) == 0 {
		// Chat-only relays commonly answer the unsupported Responses route
		// with an empty 400/422. There is no model/provider signal to preserve,
		// so one compatibility request is the useful next step.
		return true
	}
	return isExplicitProtocolFallbackMessage(string(httpErr.body))
}

// shouldFallbackToMessagesRequest is intentionally separate from the
// Responses→Chat classifier.  Messages is a third, paid protocol attempt, so
// it is selected only for a clear Chat-route rejection (or an Anthropic-shaped
// authentication mismatch where the route is known to use x-api-key).
func shouldFallbackToMessagesRequest(status int, requestErr error, key string) bool {
	if status >= 200 && status < 300 && requestErr == nil {
		return false
	}
	if errors.Is(requestErr, errInvalidUpstreamResponse) {
		return false
	}
	body := strings.ToLower(responseErrorBody(requestErr))
	if isModelUnavailableMessage(body) {
		return false
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Anthropic credentials and explicit x-api-key/messages wording are the
		// only auth cases that justify trying the Messages route.
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "sk-ant-") ||
			isExplicitMessagesProtocolMessage(body) || strings.Contains(body, "x-api-key") ||
			strings.Contains(body, "anthropic")
	}
	if isStructuredProviderFailure([]byte(body)) && !isExplicitMessagesProtocolMessage(body) {
		return false
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed ||
		status == http.StatusNotImplemented || status == http.StatusUnsupportedMediaType {
		if body == "" {
			return true
		}
		return isExplicitMessagesProtocolMessage(body) || !isStructuredProviderFailure([]byte(body))
	}
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		if body == "" {
			return true
		}
		return isExplicitMessagesProtocolMessage(body)
	}
	if status >= 500 {
		return isExplicitMessagesProtocolMessage(body)
	}
	return false
}

func isExplicitMessagesProtocolMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}
	for _, marker := range []string{
		"messages api", "messages endpoint", "/v1/messages", "anthropic messages",
		"use messages", "messages only", "message api only", "chat completions not supported",
		"chat completion endpoint not supported", "chat route unsupported",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// isStructuredProviderFailure identifies JSON error envelopes that describe a
// provider/model/auth failure rather than a missing route. It is intentionally
// conservative: malformed JSON and generic "not found" objects still allow a
// compatibility attempt, while quota/auth/upstream/model errors do not.
func isStructuredProviderFailure(body []byte) bool {
	body = trimUpstreamJSONBody(body)
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil || len(value) == 0 {
		return false
	}
	raw, ok := value["error"]
	if !ok || raw == nil {
		return false
	}
	// Inspect the error member itself as well as the whole envelope. Machine
	// codes are commonly nested under `error.code` or `error.type`, and a plain
	// string scan of the complete body must normalize those fields too.
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	message := strings.ToLower(string(rawJSON))
	normalized := strings.NewReplacer("_", " ", "-", " ").Replace(message)
	if isModelUnavailableMessage(message) {
		return true
	}
	for _, marker := range []string{
		"unauthorized", "forbidden", "permission denied", "authentication",
		"auth failed", "invalid api key", "invalid token", "api key", "credential",
		"rate limit", "ratelimit", "too many requests", "quota exceeded", "quota limit",
		"insufficient quota", "throttl", "service unavailable", "provider unavailable",
		"provider error", "temporarily unavailable", "bad gateway", "gateway timeout",
		"upstream error", "internal server error", "server error", "overloaded",
	} {
		if strings.Contains(message, marker) || strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// isExplicitProtocolFallbackMessage distinguishes an endpoint capability
// rejection from a normal model/provider error. A compatibility probe is a
// second paid request, so vague phrases such as "not supported" are not
// enough on their own (for example, a model may not support a feature while
// the Responses endpoint itself is healthy).
func isExplicitProtocolFallbackMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" || isModelUnavailableMessage(message) {
		return false
	}
	// Some relays expose a model in /v1/models but reject a Responses probe with
	// a model-capability message instead of naming the Responses route. These
	// phrases are an explicit signal that the same model must be tested through
	// Chat Completions; they are not generic model errors.
	for _, marker := range []string{
		"model only supports chat completions",
		"model only supports chat completion",
		"model supports chat completions only",
		"model supports chat completion only",
		"only supports chat completions",
		"only supports chat completion",
		"chat completions only",
		"chat completion only",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}

	unsupported := []string{
		"not implemented", "not supported", "unsupported", "not support",
	}
	responsesContext := strings.Contains(message, "responses api") ||
		strings.Contains(message, "responses endpoint") ||
		strings.Contains(message, "/v1/responses")
	if responsesContext {
		for _, marker := range unsupported {
			if strings.Contains(message, marker) {
				return true
			}
		}
	}

	// These messages describe the route/request format itself and are not
	// model availability signals. Keep the list narrow to avoid retrying on a
	// provider outage or a semantic validation error.
	for _, marker := range []string{
		"method not allowed",
		"unknown endpoint", "unknown path", "endpoint not found", "route not found",
		"unsupported endpoint", "endpoint unsupported", "unsupported route", "route unsupported",
		"invalid url", "no route", "cannot post /v1/responses", "cannot handle /v1/responses",
		"unsupported media type", "invalid content-type", "invalid content type",
		"cannot decode request", "failed to decode request", "invalid request format",
		"invalid json", "invalid json body", "failed to unmarshal request",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func classifyUpstreamTestError(ctx context.Context, status int, requestErr error) string {
	// Prefer a fully received 2xx response over a deadline that raced with the
	// final read. A non-nil transport error still takes the cancellation path
	// below, so an actually incomplete response remains a failure.
	if requestErr == nil && status >= 200 && status < 300 {
		return ""
	}
	if code := validationContextErrorCode(ctx); code != "" {
		// Preserve cancellation semantics even when the transport returned an
		// HTTP error before the context was observed by the caller.
		return code
	}
	if requestErr != nil {
		var envelope *upstreamErrorEnvelope
		if errors.As(requestErr, &envelope) {
			return classifySuccessfulErrorEnvelope(string(envelope.body))
		}
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
	case http.StatusRequestTimeout:
		return "timeout"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "auth"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "upstream"
	default:
		if status >= 500 && status <= 599 {
			return "upstream"
		}
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
	client.CheckRedirect = upstreamCheckRedirect
	account := billing.BalanceAccount{
		ID:          id,
		BaseURL:     normalizeUpstreamBaseURL(u.BaseURL),
		UpstreamKey: normalizeUpstreamKey(derefUpstreamKey(u.UpstreamKey)),
	}
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

const upstreamRedirectLimit = 4

// upstreamCheckRedirect follows the redirects emitted by ordinary relay
// front-ends (for example HTTP -> HTTPS and a trailing slash) without turning
// an upstream key into a cross-origin credential.  The default http.Client
// redirect policy is intentionally not used here: it can follow a redirect
// to another host and its POST handling can silently turn a probe into GET.
// GET catalogue/balance requests may follow a same-host redirect. POST probes
// preserve their method/body for same-origin 301/302/303 aliases in addition
// to the method-preserving 307/308 redirects handled by net/http itself.
func upstreamCheckRedirect(req *http.Request, via []*http.Request) error {
	if req == nil || req.URL == nil || len(via) == 0 {
		return http.ErrUseLastResponse
	}
	if len(via) > upstreamRedirectLimit {
		return http.ErrUseLastResponse
	}
	previous := via[len(via)-1]
	if previous == nil || previous.URL == nil {
		return http.ErrUseLastResponse
	}
	if !sameUpstreamRedirectHost(previous.URL, req.URL) {
		// The request has not been sent yet, but remove the credential from the
		// redirected copy as a defence-in-depth measure before returning the
		// original 3xx response.
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
		return http.ErrUseLastResponse
	}
	// Never follow a downgrade from HTTPS.  An initial HTTP endpoint may be
	// upgraded to HTTPS, but it must not be bounced back to cleartext.
	if strings.EqualFold(previous.URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
		return http.ErrUseLastResponse
	}
	if !strings.EqualFold(previous.URL.Scheme, req.URL.Scheme) &&
		!(strings.EqualFold(previous.URL.Scheme, "http") && strings.EqualFold(req.URL.Scheme, "https")) {
		return http.ErrUseLastResponse
	}
	// Go rewrites POST -> GET for 301/302/303 before invoking CheckRedirect.
	// Restore the original probe method/body for same-origin redirects so a
	// trailing-slash or front-end route alias does not turn a valid model into a
	// false protocol failure. The redirect policy above has already rejected
	// cross-host and HTTPS downgrade targets.
	if strings.EqualFold(previous.Method, http.MethodPost) && !strings.EqualFold(req.Method, http.MethodPost) {
		status := 0
		if req.Response != nil {
			status = req.Response.StatusCode
		}
		switch status {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther:
			if previous.GetBody == nil {
				return http.ErrUseLastResponse
			}
			body, err := previous.GetBody()
			if err != nil {
				return err
			}
			req.Method = previous.Method
			req.Body = body
			req.GetBody = previous.GetBody
			req.ContentLength = previous.ContentLength
			restoreRedirectBodyHeaders(req.Header, previous.Header)
		default:
			return http.ErrUseLastResponse
		}
	}
	return nil
}

func restoreRedirectBodyHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for _, name := range []string{"Content-Type", "Content-Encoding", "Content-Language", "Content-Location"} {
		values := src.Values(name)
		if len(values) == 0 {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
}

func sameUpstreamRedirectHost(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	if !strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	leftPort := redirectPort(left)
	rightPort := redirectPort(right)
	if leftPort == rightPort {
		return true
	}
	// An explicit standard port is equivalent to the implicit one. During the
	// only permitted scheme change, also accept the standard 80 -> 443 upgrade
	// used by reverse proxies. Custom-port redirects remain same-origin only
	// when the port itself is unchanged.
	return strings.EqualFold(left.Scheme, "http") && strings.EqualFold(right.Scheme, "https") && leftPort == "80" && rightPort == "443"
}

func redirectPort(value *url.URL) string {
	if value == nil {
		return ""
	}
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// normalizeUpstreamBaseURL accepts both the documented bare root and commonly
// copied API operation URLs. The gateway itself appends /v1 and the operation
// path, so retaining a pasted `/v1/chat/completions`, `/v1/responses`, or
// `/v1/messages` suffix would probe a path such as `/v1/chat/completions/v1/models`
// and make an otherwise valid relay look dead. Prefixes such as `/openai` are
// kept intact.
func normalizeUpstreamBaseURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Path == "" {
		return base
	}
	path := strings.TrimRight(parsed.Path, "/")
	lower := strings.ToLower(path)
	strippedOperation := false
	for _, suffix := range []string{"/chat/completions", "/responses", "/messages"} {
		if strings.HasSuffix(lower, suffix) {
			path = path[:len(path)-len(suffix)]
			lower = strings.ToLower(strings.TrimRight(path, "/"))
			strippedOperation = true
			break
		}
	}
	if lower == "/v1" || strings.HasSuffix(lower, "/v1") {
		path = path[:len(path)-len("/v1")]
		if path == "/" {
			path = ""
		}
		strippedOperation = true
	}
	if strippedOperation {
		parsed.Path = path
		parsed.RawPath = ""
		base = parsed.String()
	}
	return strings.TrimRight(base, "/")
}

// upstreamURL joins a normalized base and a protocol path without letting a
// leading slash discard a base path (for example https://relay/openai).
func upstreamURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	path = strings.TrimLeft(path, "/")
	if path == "" {
		return base
	}
	return base + "/" + path
}

func validateUpstream(u *domain.Upstream) error {
	if u == nil || strings.TrimSpace(u.Name) == "" || len(strings.TrimSpace(u.Name)) > 200 {
		return ErrInvalidInput
	}
	u.Name = strings.TrimSpace(u.Name)
	u.BaseURL = normalizeUpstreamBaseURL(u.BaseURL)
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
		key := normalizeUpstreamKey(*u.UpstreamKey)
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
	return key != nil && normalizeUpstreamKey(*key) != ""
}

func upstreamKeysDiffer(left, right *string) bool {
	leftValue, rightValue := "", ""
	if left != nil {
		leftValue = normalizeUpstreamKey(*left)
	}
	if right != nil {
		rightValue = normalizeUpstreamKey(*right)
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
	return normalizeUpstreamKey(*key)
}

// normalizeUpstreamKey accepts the common copy/paste form "Bearer <key>"
// while storing and forwarding only the credential value. Repeated prefixes
// are collapsed as well, preventing a historical or imported value from
// becoming "Bearer Bearer ..." on the wire.
func normalizeUpstreamKey(value string) string {
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
