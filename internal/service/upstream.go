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
	"sort"
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
	upstreamValidationConcurrency      = 2
	upstreamValidationBatchLimit       = 200
	// Bound the synchronous admin action as a whole. Per-upstream deadlines are
	// not sufficient when an inventory contains many slow or unreachable
	// relays: without this cap the HTTP request and the shared validation lock
	// could remain occupied for hours.
	upstreamValidationBatchTimeout = 5 * time.Minute
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
)

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
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	result := validateUpstreamModels(ctx, client, base, key)
	if !result.ValidationComplete || !result.OK {
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
// credential changed only after the new connection has passed a complete
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
		client.Timeout = 10 * time.Second
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		result := validateUpstreamModels(ctx, client, base, key)
		if !result.ValidationComplete || !result.OK {
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
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
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
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	key := normalizeUpstreamKey(derefUpstreamKey(u.UpstreamKey))
	result := validateUpstreamModels(ctx, client, base, key)
	if err := s.recordUpstreamModels(ctx, store, u, result.Models, result.ErrorCode, result.ValidationComplete); err != nil {
		return nil, mapRepoErr(err)
	}
	return &result, nil
}

// ValidateAllUpstreams performs the same complete model validation used by the
// add/edit flow for every saved upstream. Each row is checked independently so
// one broken relay cannot abort the rest of the operation; the returned order
// is stable by upstream ID and every completed probe records health telemetry.
func (s *Service) ValidateAllUpstreams(ctx context.Context) (*UpstreamValidationSummary, error) {
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
	if len(rows) == 0 {
		return &UpstreamValidationSummary{Items: []UpstreamValidationItem{}, DurationMS: time.Since(started).Milliseconds()}, nil
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
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				items[j.index] = s.validateStoredUpstream(validationCtx, store, j.row)
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
	return summarizeUpstreamValidation(items, started), nil
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
	probeCtx, cancel := context.WithTimeout(ctx, upstreamModelValidationTimeout)
	defer cancel()
	client := s.managementHTTPClient()
	client.Timeout = upstreamModelValidationTimeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	started := time.Now()
	result := validateUpstreamModels(probeCtx, client, base, normalizeUpstreamKey(derefUpstreamKey(expected.UpstreamKey)))
	item.LatencyMS = time.Since(started).Milliseconds()
	item.Models = append([]string{}, result.Models...)
	item.ModelsTotal = result.ModelsTotal
	item.ModelsChecked = result.ModelsChecked
	item.ModelsAvailable = result.ModelsAvailable
	item.ModelsFailed = result.ModelsFailed
	item.ValidationComplete = result.ValidationComplete
	item.OK = result.ValidationComplete && result.OK
	item.ErrorCode = result.ErrorCode
	if item.ErrorCode == "" && !item.OK {
		item.ErrorCode = "model_unavailable"
	}
	// An incomplete run must not replace the last verified snapshot. A complete
	// run, including an empty or definitively rejected one, writes the exact
	// result.
	persistCtx, persistCancel := upstreamValidationPersistenceContext(ctx)
	defer persistCancel()
	if err := s.recordUpstreamModels(persistCtx, store, expected, result.Models, result.ErrorCode, result.ValidationComplete); err != nil {
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

func (s *Service) recordUpstreamModels(ctx context.Context, store UpstreamStore, expected *domain.Upstream, models []string, code string, complete bool) error {
	recorder, ok := store.(UpstreamModelStore)
	if !ok {
		return nil
	}
	var modelErr *string
	if code != "" {
		modelErr = &code
		if !complete {
			// A catalogue/transport failure or an overall deadline means the
			// current run did not check every advertised model. Keep the last
			// verified catalogue while exposing the transient error.
			models = nil
		}
	}
	if _, err := recorder.RecordUpstreamModels(ctx, expected, models, modelErr); err != nil {
		// A concurrent endpoint/key edit makes the probe stale. The read result is
		// still useful to the caller, while the next reload will use the new config.
		return err
	}
	s.invalidateUpstreamConfig(ctx)
	return nil
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
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	result := validateUpstreamModels(ctx, client, base, normalizeUpstreamKey(key))
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
	testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := s.managementHTTPClient()
	client.Timeout = 12 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	key := normalizeUpstreamKey(derefUpstreamKey(u.UpstreamKey))
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
	status, requestErr := sendUpstreamTestRequest(testCtx, client, upstreamURL(base, "/v1/responses"), key, model, false)
	if shouldFallbackTestRequest(status, requestErr) && testCtx.Err() == nil {
		status, requestErr = sendUpstreamTestRequest(testCtx, client, upstreamURL(base, "/v1/chat/completions"), key, model, true)
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
	key = normalizeUpstreamKey(key)
	// OpenAI-compatible relays usually expose /v1/models, but a number of
	// gateways expose the same catalogue at /models when their configured base
	// URL already represents the versioned API root. Try that route only when
	// the canonical route explicitly looks missing; auth, network, rate-limit,
	// and provider failures must retain their original classification.
	paths := []string{"/v1/models", "/models"}
	lastCode := "http_error"
	for i, path := range paths {
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		models, code, status, retryable := fetchAdvertisedModelsPath(requestCtx, client, upstreamURL(base, path), key)
		cancel()
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

// fetchAdvertisedModelsPath performs one bounded catalogue request. The
// boolean return identifies route-level failures for which the caller may try
// a compatibility path; it deliberately excludes model/auth/provider errors.
func fetchAdvertisedModelsPath(ctx context.Context, client *http.Client, target, key string) ([]string, string, int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "invalid_value", 0, false
	}
	key = normalizeUpstreamKey(key)
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
		// Keep a small bounded copy for 404 classification. Some gateways use a
		// JSON error envelope on a missing model/tenant; treating that as a
		// missing route would trigger a second request against the compatibility
		// path and can duplicate a charge. Plain-text/HTML router pages remain
		// route-level signals and keep the fallback behavior.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, "auth", resp.StatusCode, false
		case http.StatusTooManyRequests:
			return nil, "rate_limited", resp.StatusCode, false
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return nil, "upstream", resp.StatusCode, false
		case http.StatusNotFound:
			return nil, "http_error", resp.StatusCode, !isStructuredProviderFailure(body)
		case http.StatusMethodNotAllowed, http.StatusNotImplemented:
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
	models, recognized := parseUpstreamModelsPayload(body)
	if !recognized {
		if code, ok := classifyModelCatalogueErrorEnvelope(body); ok {
			return nil, code, resp.StatusCode, false
		}
		return nil, "invalid_value", resp.StatusCode, false
	}
	if len(models) == 0 {
		// An explicitly valid but empty catalogue is a completed capability
		// check. Keep it distinct from malformed JSON so callers clear a stale
		// model snapshot instead of continuing to route to old models.
		return []string{}, "model_unavailable", resp.StatusCode, false
	}
	return models, "", resp.StatusCode, false
}

// classifyModelCatalogueErrorEnvelope handles relays that return an
// OpenAI-style error object with HTTP 200 for a failed /models request. It is
// deliberately limited to a top-level error member; arbitrary JSON remains an
// invalid catalogue instead of being guessed as an auth or outage signal.
func classifyModelCatalogueErrorEnvelope(body []byte) (string, bool) {
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
}

// validateUpstreamModels turns an advertised catalogue into a verified
// catalogue. Every model receives one tiny non-streaming request, bounded by a
// worker pool and per-model/overall deadlines. The route is Responses first;
// only an explicit protocol/format rejection is retried as Chat Completions.
func validateUpstreamModels(ctx context.Context, client *http.Client, base, key string) UpstreamModelsResult {
	models, code := fetchAdvertisedModels(ctx, client, base, key)
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
	}
}

func validateModelCatalogue(ctx context.Context, client *http.Client, base, key string, models []string) upstreamModelValidation {
	return validateModelCatalogueWithTimeout(ctx, client, base, key, models, upstreamModelValidationTimeout)
}

// validateModelCatalogueWithTimeout contains the bounded catalogue worker and
// accepts a timeout override so the deadline boundary can be tested without
// waiting for the production 30-second limit.
func validateModelCatalogueWithTimeout(ctx context.Context, client *http.Client, base, key string, models []string, timeout time.Duration) upstreamModelValidation {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(models) == 0 {
		return upstreamModelValidation{ValidationComplete: true}
	}
	validationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type outcome struct {
		model   string
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
				status, requestErr := sendUpstreamTestRequest(modelCtx, client, upstreamURL(base, "/v1/responses"), key, model, false)
				if shouldFallbackTestRequest(status, requestErr) && modelCtx.Err() == nil {
					status, requestErr = sendUpstreamTestRequest(modelCtx, client, upstreamURL(base, "/v1/chat/completions"), key, model, true)
				}
				modelCancel()
				modelCode := classifyModelValidationError(validationCtx, status, requestErr)
				recordFatal(modelCode)
				results <- outcome{model: model, ok: modelCode == "", checked: true, code: modelCode}
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
	// An account-wide authentication signal can stop dispatching new work. It
	// must not turn a run that already checked every advertised model into a
	// partial result (for example a one-model catalogue returning auth). A caller
	// deadline/cancellation or this function's own bounded deadline makes an
	// otherwise complete-looking run incomplete.
	complete := checked == len(models) && validationCtx.Err() == nil
	// A catalogue with no successful model and any transient transport or
	// throttling failure is not a trustworthy "no models" snapshot. Keep the
	// previous verified list so one provider hiccup cannot remove every route.
	// An authentication failure remains authoritative: an invalid credential
	// should clear the stale snapshot instead of routing traffic into it.
	if complete && len(ordered) == 0 && counts["auth"] == 0 && hasTransientValidationFailure(counts) {
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
	}
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

func classifyModelValidationError(ctx context.Context, status int, requestErr error) string {
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
	for _, marker := range []string{"model not found", "model_not_found", "unknown model", "invalid model", "model does not exist", "no such model", "model unavailable"} {
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
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false
	}
	var entries []any
	recognized := false
	switch value := root.(type) {
	case []any:
		entries = value
		recognized = true
	case map[string]any:
		// A paginated response is not a complete catalogue. This validator does
		// not guess cursor semantics across providers; fail closed so the caller
		// cannot publish or route against only the first page.
		if hasMore, ok := value["has_more"].(bool); ok && hasMore {
			return nil, false
		}
		for _, key := range []string{"next", "next_page", "next_cursor"} {
			if cursor, ok := value[key].(string); ok && strings.TrimSpace(cursor) != "" {
				return nil, false
			}
		}
		for _, key := range []string{"data", "models"} {
			if candidate, ok := value[key].([]any); ok {
				entries = candidate
				recognized = true
				break
			}
		}
	}
	if !recognized {
		return nil, false
	}
	if len(entries) > upstreamModelCatalogueMax {
		return nil, false
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
			for _, key := range []string{"id", "name", "model"} {
				if candidate, ok := value[key].(string); ok {
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
			return nil, false
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		models = append(models, name)
	}
	return models, true
}

func containsModel(models []string, target string) bool {
	for _, model := range models {
		if model == target {
			return true
		}
	}
	return false
}

func sendUpstreamTestRequest(ctx context.Context, client *http.Client, target, key, model string, chat bool) (int, error) {
	key = normalizeUpstreamKey(key)
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
	if object, ok := value["object"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(object)) {
		case "chat.completion", "chat.completion.chunk", "completion", "text_completion", "response", "message", "image", "image_generation":
			return true
		}
	}
	if choices, ok := value["choices"].([]any); ok {
		return len(choices) > 0
	}
	if output, ok := value["output"].([]any); ok {
		return len(output) > 0
	}
	switch data := value["data"].(type) {
	case []any:
		return len(data) > 0
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
		if json.Unmarshal([]byte(message), &structured) != nil {
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
	return isExplicitProtocolFallbackMessage(string(httpErr.body))
}

// isStructuredProviderFailure identifies JSON error envelopes that describe a
// provider/model/auth failure rather than a missing route. It is intentionally
// conservative: malformed JSON and generic "not found" objects still allow a
// compatibility attempt, while quota/auth/upstream/model errors do not.
func isStructuredProviderFailure(body []byte) bool {
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

// normalizeUpstreamBaseURL accepts both the documented bare root and the
// commonly copied /v1 endpoint. The gateway itself appends /v1, so retaining
// that suffix would probe /v1/v1 and make an otherwise valid relay look dead.
// Only a final /v1 path segment is removed; prefixes such as /openai are kept.
func normalizeUpstreamBaseURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Path == "" {
		return base
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
