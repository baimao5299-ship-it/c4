// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// upstreamTestStore embeds the existing service.Store fake and supplies only the
// optional upstream-management surface. Keeping it local to this test avoids
// widening the production Store interface or changing unrelated test fixtures.
type upstreamTestStore struct {
	*fakeStore
	upstreams map[int64]*domain.Upstream
}

func newUpstreamTestStore() *upstreamTestStore {
	return &upstreamTestStore{fakeStore: newFakeStore(), upstreams: make(map[int64]*domain.Upstream)}
}

// revisionRaceStore changes the persisted row after the handler's first GET.
// The service performs its own second GET during an update, so this isolates
// the race that a legacy form (which omits expected_updated_at) must reject.
type revisionRaceStore struct {
	*upstreamTestStore
	gets atomic.Int32
}

func (s *revisionRaceStore) GetUpstream(ctx context.Context, id int64) (*domain.Upstream, error) {
	row, err := s.upstreamTestStore.GetUpstream(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.gets.Add(1) == 1 {
		s.mu.Lock()
		if stored := s.upstreams[id]; stored != nil {
			stored.MultiplierBP = 7777
			stored.UpdatedAt = stored.UpdatedAt.Add(time.Minute)
		}
		s.mu.Unlock()
	}
	return row, nil
}

func cloneUpstream(in *domain.Upstream) *domain.Upstream {
	if in == nil {
		return nil
	}
	out := *in
	if in.UpstreamKey != nil {
		v := *in.UpstreamKey
		out.UpstreamKey = &v
	}
	if in.Note != nil {
		v := *in.Note
		out.Note = &v
	}
	if in.BalanceAmount != nil {
		v := *in.BalanceAmount
		out.BalanceAmount = &v
	}
	if in.BalanceCurrency != nil {
		v := *in.BalanceCurrency
		out.BalanceCurrency = &v
	}
	if in.LastError != nil {
		v := *in.LastError
		out.LastError = &v
	}
	if in.Models != nil {
		out.Models = append([]string(nil), in.Models...)
	}
	if in.ModelsCheckedAt != nil {
		v := *in.ModelsCheckedAt
		out.ModelsCheckedAt = &v
	}
	if in.ModelsError != nil {
		v := *in.ModelsError
		out.ModelsError = &v
	}
	return &out
}

func (s *upstreamTestStore) CreateUpstream(_ context.Context, in *domain.Upstream) (*domain.Upstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.upstreams {
		if row.DeletedAt == nil && row.Name == in.Name {
			return nil, fmt.Errorf("%w: name=%q", repository.ErrConflict, in.Name)
		}
	}
	in = cloneUpstream(in)
	in.ID = s.nextID
	s.nextID++
	now := time.Now()
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now
	}
	if in.UpdatedAt.IsZero() {
		in.UpdatedAt = now
	}
	if in.BalanceStatus == "" {
		in.BalanceStatus = domain.UpstreamBalanceUnconfigured
	}
	s.upstreams[in.ID] = in
	return cloneUpstream(in), nil
}

func (s *upstreamTestStore) GetUpstream(_ context.Context, id int64) (*domain.Upstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.upstreams[id]
	if !ok || row.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	return cloneUpstream(row), nil
}

func (s *upstreamTestStore) ListUpstreams(_ context.Context, q repository.ListQuery) ([]*domain.Upstream, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]*domain.Upstream, 0, len(s.upstreams))
	for _, row := range s.upstreams {
		if row.DeletedAt != nil {
			continue
		}
		if q.Name != "" && !strings.Contains(strings.ToLower(row.Name), strings.ToLower(q.Name)) {
			continue
		}
		if len(q.StatusList) > 0 {
			matched := false
			for _, status := range q.StatusList {
				if (status == "active" && row.Enabled) || (status == "disabled" && !row.Enabled) {
					matched = true
				}
			}
			if !matched {
				continue
			}
		}
		rows = append(rows, cloneUpstream(row))
	}
	total := int64(len(rows))
	if q.Offset < 0 {
		q.Offset = 0
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset >= len(rows) {
		return []*domain.Upstream{}, total, nil
	}
	end := q.Offset + q.Limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[q.Offset:end], total, nil
}

func (s *upstreamTestStore) UpdateUpstream(_ context.Context, in *domain.Upstream) (*domain.Upstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.upstreams[in.ID]
	if !ok || row.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, in.ID)
	}
	if in.ExpectedUpdatedAt != nil && !row.UpdatedAt.Equal(*in.ExpectedUpdatedAt) {
		return nil, fmt.Errorf("%w: id=%d changed", repository.ErrConflict, in.ID)
	}
	for id, other := range s.upstreams {
		if id != in.ID && other.DeletedAt == nil && other.Name == in.Name {
			return nil, fmt.Errorf("%w: name=%q", repository.ErrConflict, in.Name)
		}
	}
	in = cloneUpstream(in)
	if in.Note != nil && strings.TrimSpace(*in.Note) == "" {
		in.Note = nil
	}
	in.CreatedAt = row.CreatedAt
	in.UpdatedAt = time.Now()
	if !in.UpdatedAt.After(row.UpdatedAt) {
		in.UpdatedAt = row.UpdatedAt.Add(time.Nanosecond)
	}
	in.DeletedAt = row.DeletedAt
	s.upstreams[in.ID] = in
	return cloneUpstream(in), nil
}

func (s *upstreamTestStore) SetUpstreamEnabled(_ context.Context, id int64, enabled bool) (*domain.Upstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.upstreams[id]
	if !ok || row.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	row.Enabled = enabled
	row.UpdatedAt = time.Now()
	return cloneUpstream(row), nil
}

func (s *upstreamTestStore) DeleteUpstream(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.upstreams[id]
	if !ok || row.DeletedAt != nil {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	now := time.Now()
	row.DeletedAt = &now
	return nil
}

func (s *upstreamTestStore) RecordUpstreamProbe(_ context.Context, expected *domain.Upstream, success bool, latencyMS int64, probeErr *string) (*domain.Upstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected == nil {
		return nil, repository.ErrNotFound
	}
	row, ok := s.upstreams[expected.ID]
	if !ok || row.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, expected.ID)
	}
	if !sameUpstreamConfig(row, expected, false) {
		return nil, fmt.Errorf("%w: id=%d configuration changed", repository.ErrConflict, expected.ID)
	}
	now := time.Now()
	row.RequestCount++
	if latencyMS > 0 {
		row.LatencyTotalMS += latencyMS
		if latencyMS > row.LatencyMaxMS {
			row.LatencyMaxMS = latencyMS
		}
	}
	row.LastCheckedAt = &now
	if success {
		row.SuccessCount++
		row.LastSuccessAt = &now
		row.LastError = nil
	} else {
		row.FailureCount++
		row.LastFailureAt = &now
		row.LastError = probeErr
	}
	return cloneUpstream(row), nil
}

func (s *upstreamTestStore) RecordUpstreamBalance(_ context.Context, expected *domain.Upstream, amount, currency *string, status string, checkedAt *time.Time) (*domain.Upstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected == nil {
		return nil, repository.ErrNotFound
	}
	row, ok := s.upstreams[expected.ID]
	if !ok || row.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, expected.ID)
	}
	if !sameUpstreamConfig(row, expected, true) {
		return nil, fmt.Errorf("%w: id=%d configuration changed", repository.ErrConflict, expected.ID)
	}
	row.BalanceAmount = amount
	row.BalanceCurrency = currency
	row.BalanceStatus = status
	row.BalanceCheckedAt = checkedAt
	return cloneUpstream(row), nil
}

func (s *upstreamTestStore) RecordUpstreamModels(_ context.Context, expected *domain.Upstream, models []string, modelErr *string) (*domain.Upstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected == nil {
		return nil, repository.ErrNotFound
	}
	row, ok := s.upstreams[expected.ID]
	if !ok || row.DeletedAt != nil {
		return nil, fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, expected.ID)
	}
	if !sameUpstreamConfig(row, expected, false) {
		return nil, fmt.Errorf("%w: id=%d configuration changed", repository.ErrConflict, expected.ID)
	}
	if !(modelErr != nil && models == nil) {
		row.Models = append([]string(nil), models...)
		now := time.Now()
		row.ModelsCheckedAt = &now
	}
	row.ModelsError = modelErr
	return cloneUpstream(row), nil
}

func sameUpstreamConfig(current, expected *domain.Upstream, includeBalance bool) bool {
	if current == nil || expected == nil || current.BaseURL != expected.BaseURL || upstreamTestKey(current.UpstreamKey) != upstreamTestKey(expected.UpstreamKey) {
		return false
	}
	if !includeBalance {
		return true
	}
	return current.BalanceEndpoint == expected.BalanceEndpoint &&
		current.BalanceMethod == expected.BalanceMethod &&
		current.BalanceAuth == expected.BalanceAuth &&
		current.BalancePath == expected.BalancePath &&
		current.BalanceCurrencyPath == expected.BalanceCurrencyPath
}

func upstreamTestKey(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

var _ service.Store = (*upstreamTestStore)(nil)
var _ service.UpstreamStore = (*upstreamTestStore)(nil)
var _ service.UpstreamModelStore = (*upstreamTestStore)(nil)

func TestUpstreamManagementLifecycleAndSecretBoundary(t *testing.T) {
	store := newUpstreamTestStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc)
	r := chi.NewRouter()
	r.Mount("/", h.Router())

	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer secret-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6"}]}`))
		case "/v1/responses":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"response-1","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer probe.Close()

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/api/admin/upstreams", fmt.Sprintf(`{"name":"relay-a","base_url":%q,"upstream_key":"secret-key","multiplier_bp":12500,"enabled":true}`, probe.URL))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "secret-key", "API response must never expose the key")
	require.Contains(t, rec.Body.String(), `"CredentialConfigured":true`)
	require.Contains(t, rec.Body.String(), `"Multiplier":1.25`)

	var created Upstream
	require.NoError(t, jsonUnmarshal(rec.Body.Bytes(), &created))

	rec = do(http.MethodPut, "/api/admin/upstreams/"+itoa(created.ID), fmt.Sprintf(`{"name":"relay-a-renamed","base_url":%q,"multiplier_bp":8000,"enabled":false}`, probe.URL))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "secret-key")
	require.Contains(t, rec.Body.String(), `"Multiplier":0.8`)
	require.Contains(t, rec.Body.String(), `"Status":"disabled"`)
	require.Contains(t, rec.Body.String(), `"CredentialConfigured":true`, "omitting key on PUT must retain it")

	rec = do(http.MethodPatch, "/api/admin/upstreams/"+itoa(created.ID)+"/status", `{"enabled":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"Status":"active"`)
	require.Contains(t, rec.Body.String(), `"Multiplier":0.8`, "status updates must not overwrite the multiplier")

	rec = do(http.MethodPut, "/api/admin/upstreams/"+itoa(created.ID), fmt.Sprintf(`{"name":"relay-a-renamed","base_url":%q,"note":""}`, probe.URL))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"Note":null`, "an explicit empty note clears the stored note")

	rec = do(http.MethodPost, "/api/admin/upstreams/"+itoa(created.ID)+"/probe", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"ok":true`)
	require.Contains(t, rec.Body.String(), `"RequestCount":1`)

	rec = do(http.MethodPost, "/api/admin/upstreams/"+itoa(created.ID)+"/balance", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"error_code":"unconfigured"`)
	require.Contains(t, rec.Body.String(), `"BalanceStatus":"unconfigured"`)

	rec = do(http.MethodDelete, "/api/admin/upstreams/"+itoa(created.ID), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/upstreams", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"total":0`)
}

func TestUpstreamModelsPreviewAndSelectedTest(t *testing.T) {
	key := "preview-key"
	var testedModel string
	var modelListCalls int32
	var modelProbeCalls int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/models":
			atomic.AddInt32(&modelListCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
		case "/v1/responses":
			atomic.AddInt32(&modelProbeCalls, 1)
			var body struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			testedModel = body.Model
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"ok","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	store := newUpstreamTestStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	svc.SetUpstreamHTTPClient(endpoint.Client())
	h := New(svc)
	router := chi.NewRouter()
	router.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/api/admin/upstreams/models", fmt.Sprintf(`{"base_url":%q,"upstream_key":%q}`, endpoint.URL, key))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var preview UpstreamModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	require.True(t, preview.Ok)
	require.Equal(t, []string{"model-a", "model-b"}, preview.Models)
	require.Equal(t, 2, preview.ModelsTotal)
	require.Equal(t, 2, preview.ModelsChecked)
	require.Equal(t, 2, preview.ModelsAvailable)
	require.Zero(t, preview.ModelsFailed)
	require.True(t, preview.ValidationComplete)

	rec = do(http.MethodPost, "/api/admin/upstreams", fmt.Sprintf(`{"name":"preview-relay","base_url":%q,"upstream_key":%q}`, endpoint.URL, key))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created Upstream
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.Models)
	require.Equal(t, []string{"model-a", "model-b"}, *created.Models)
	require.Equal(t, int32(2), atomic.LoadInt32(&modelListCalls), "create must validate once after the explicit preview")
	require.Equal(t, int32(4), atomic.LoadInt32(&modelProbeCalls), "create must not repeat a second validation pass")
	require.NotNil(t, created.ModelsCheckedAt)
	rec = do(http.MethodGet, "/api/admin/upstreams/"+itoa(created.ID)+"/models", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/upstreams/"+itoa(created.ID)+"/test", `{"model":"model-b"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "model-b", testedModel)
	rec = do(http.MethodPost, "/api/admin/upstreams/validate-all", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var batch UpstreamValidationSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &batch))
	require.Equal(t, 1, batch.Total)
	require.Equal(t, 1, batch.Completed)
	require.Equal(t, 1, batch.Passed)
	require.Zero(t, batch.Failed)
	require.Len(t, batch.Items, 1)
	require.True(t, batch.Items[0].Ok)
}

func TestUpstreamUpdateRejectsStaleRevision(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"revision-model"}]}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"revision-response","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()
	store := newUpstreamTestStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	svc.SetUpstreamHTTPClient(endpoint.Client())
	h := New(svc)
	r := chi.NewRouter()
	r.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/api/admin/upstreams", fmt.Sprintf(`{"name":"revision-relay","base_url":%q,"multiplier_bp":10000}`, endpoint.URL))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created Upstream
	require.NoError(t, jsonUnmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.UpdatedAt)
	version := created.UpdatedAt.Format(time.RFC3339Nano)

	rec = do(http.MethodPut, "/api/admin/upstreams/"+itoa(created.ID), fmt.Sprintf(`{"name":"revision-relay","base_url":%q,"multiplier_bp":9000,"expected_updated_at":%q}`, endpoint.URL, version))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// An explicitly supplied revision remains an optimistic-concurrency
	// contract, even for a multiplier-only edit.
	rec = do(http.MethodPut, "/api/admin/upstreams/"+itoa(created.ID), fmt.Sprintf(`{"name":"revision-relay","base_url":%q,"multiplier_bp":8000,"expected_updated_at":%q}`, endpoint.URL, version))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	current, err := store.GetUpstream(context.Background(), created.ID)
	require.NoError(t, err)
	require.Equal(t, 9000, current.MultiplierBP)
	require.Nil(t, current.UpstreamKey)
}

func TestPutUpstreamLegacyBodyUsesFirstReadRevision(t *testing.T) {
	var modelProbes atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"revision-model"}]}`))
		case "/v1/responses":
			modelProbes.Add(1)
			_, _ = w.Write([]byte(`{"id":"revision-response","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	base := newUpstreamTestStore()
	store := &revisionRaceStore{upstreamTestStore: base}
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	svc.SetUpstreamHTTPClient(endpoint.Client())
	h := New(svc)
	r := chi.NewRouter()
	r.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/api/admin/upstreams", fmt.Sprintf(`{"name":"legacy-race","base_url":%q,"upstream_key":"old-key"}`, endpoint.URL))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created Upstream
	require.NoError(t, jsonUnmarshal(rec.Body.Bytes(), &created))

	// The body intentionally omits expected_updated_at. The wrapper mutates the
	// row immediately after the handler's GET and before the service's GET. The
	// first-read revision must therefore reject the stale credential edit before
	// it performs another (potentially billable) model probe.
	rec = do(http.MethodPut, "/api/admin/upstreams/"+itoa(created.ID), fmt.Sprintf(`{"name":"legacy-race","base_url":%q,"upstream_key":"new-key","multiplier_bp":9000}`, endpoint.URL))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, int32(1), modelProbes.Load(), "stale legacy edit must not probe the replacement key")

	current, err := base.GetUpstream(context.Background(), created.ID)
	require.NoError(t, err)
	require.Equal(t, 7777, current.MultiplierBP, "the concurrent revision must remain intact")
	require.NotNil(t, current.UpstreamKey)
	require.Equal(t, "old-key", *current.UpstreamKey)
}

func TestCanonicalUpstreamKeyTreatsBearerCopyFormsAsEqual(t *testing.T) {
	for _, tc := range []struct {
		name  string
		left  string
		right string
	}{
		{name: "plain whitespace", left: " relay-key ", right: "relay-key"},
		{name: "single bearer", left: "Bearer relay-key", right: "relay-key"},
		{name: "repeated bearer", left: "Bearer Bearer relay-key", right: "relay-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, canonicalUpstreamKey(tc.right), canonicalUpstreamKey(tc.left))
		})
	}
	require.NotEqual(t, canonicalUpstreamKey("relay-key-a"), canonicalUpstreamKey("relay-key-b"))
}

func TestAPIUpstreamHidesLegacyBalanceWhenUnconfigured(t *testing.T) {
	amount := "9.99"
	currency := "USD"
	u := &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: "https://relay.example.com",
		BalanceStatus: domain.UpstreamBalanceUnconfigured,
		BalanceAmount: &amount, BalanceCurrency: &currency,
	}
	got := toAPIUpstream(u)
	require.Equal(t, UpstreamBalanceStatusUnconfigured, got.BalanceStatus)
	require.Nil(t, got.BalanceAmount)
	require.Nil(t, got.BalanceCurrency)
}

func TestUpstreamBalanceRefreshReturnsFreshThenStaleSnapshot(t *testing.T) {
	respondSuccess := true
	balance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer relay-key", r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"balance-model"}]}`))
			return
		}
		if r.URL.Path == "/v1/responses" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"balance-response","object":"response"}`))
			return
		}
		require.Equal(t, http.MethodGet, r.Method)
		if !respondSuccess {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"remaining":"7.25","currency":"USD"}}`))
	}))
	defer balance.Close()

	store := newUpstreamTestStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc)
	r := chi.NewRouter()
	r.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/api/admin/upstreams", fmt.Sprintf(`{"name":"relay-balance","base_url":%q,"upstream_key":"relay-key","balance_endpoint":%q,"balance_method":"GET","balance_auth":"bearer","balance_path":"data.remaining","balance_currency_path":"data.currency"}`, balance.URL, balance.URL))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created Upstream
	require.NoError(t, jsonUnmarshal(rec.Body.Bytes(), &created))

	rec = do(http.MethodPost, "/api/admin/upstreams/"+itoa(created.ID)+"/balance", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var fresh UpstreamProbeResponse
	require.NoError(t, jsonUnmarshal(rec.Body.Bytes(), &fresh))
	require.True(t, fresh.Ok)
	require.Nil(t, fresh.ErrorCode)
	require.Equal(t, UpstreamBalanceStatusFresh, fresh.Upstream.BalanceStatus)
	require.NotNil(t, fresh.Upstream.BalanceAmount)
	require.Equal(t, "7.25", *fresh.Upstream.BalanceAmount)
	require.NotNil(t, fresh.Upstream.BalanceCheckedAt)

	respondSuccess = false
	rec = do(http.MethodPost, "/api/admin/upstreams/"+itoa(created.ID)+"/balance", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var stale UpstreamProbeResponse
	require.NoError(t, jsonUnmarshal(rec.Body.Bytes(), &stale))
	require.False(t, stale.Ok)
	require.NotNil(t, stale.ErrorCode)
	require.Equal(t, UpstreamProbeResponseErrorCodeUpstream, *stale.ErrorCode)
	require.Equal(t, UpstreamBalanceStatusStale, stale.Upstream.BalanceStatus)
	require.NotNil(t, stale.Upstream.BalanceAmount)
	require.Equal(t, "7.25", *stale.Upstream.BalanceAmount)
	require.NotNil(t, stale.Upstream.BalanceCheckedAt)
}

// jsonUnmarshal is a tiny local helper to keep this test independent of the
// domain JSON tags, which intentionally differ from the management API casing.
func jsonUnmarshal(data []byte, out any) error {
	return json.Unmarshal(data, out)
}
