// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

type upstreamServiceStub struct {
	row             *domain.Upstream
	recordProbeFn   func(*domain.Upstream, bool, int64, *string) (*domain.Upstream, error)
	recordBalanceFn func(*domain.Upstream, *string, *string, string, *time.Time) (*domain.Upstream, error)
	recordModelsFn  func(*domain.Upstream, []string, *string) (*domain.Upstream, error)
}

func (s *upstreamServiceStub) CreateUpstream(_ context.Context, in *domain.Upstream) (*domain.Upstream, error) {
	return in, nil
}

func (s *upstreamServiceStub) GetUpstream(_ context.Context, _ int64) (*domain.Upstream, error) {
	if s.row == nil {
		return nil, errors.New("missing")
	}
	copy := *s.row
	return &copy, nil
}

func (s *upstreamServiceStub) ListUpstreams(_ context.Context, _ repository.ListQuery) ([]*domain.Upstream, int64, error) {
	if s.row == nil {
		return nil, 0, nil
	}
	return []*domain.Upstream{s.row}, 1, nil
}

func (s *upstreamServiceStub) UpdateUpstream(_ context.Context, in *domain.Upstream) (*domain.Upstream, error) {
	return in, nil
}

func (s *upstreamServiceStub) SetUpstreamEnabled(_ context.Context, id int64, enabled bool) (*domain.Upstream, error) {
	if s.row == nil || s.row.ID != id || s.row.DeletedAt != nil {
		return nil, repository.ErrNotFound
	}
	s.row.Enabled = enabled
	copy := *s.row
	return &copy, nil
}

func (s *upstreamServiceStub) DeleteUpstream(_ context.Context, _ int64) error { return nil }

func (s *upstreamServiceStub) RecordUpstreamProbe(_ context.Context, expected *domain.Upstream, success bool, latencyMS int64, probeErr *string) (*domain.Upstream, error) {
	if s.recordProbeFn != nil {
		return s.recordProbeFn(expected, success, latencyMS, probeErr)
	}
	return s.row, nil
}

func (s *upstreamServiceStub) RecordUpstreamBalance(_ context.Context, expected *domain.Upstream, amount, currency *string, status string, checkedAt *time.Time) (*domain.Upstream, error) {
	if s.recordBalanceFn != nil {
		return s.recordBalanceFn(expected, amount, currency, status, checkedAt)
	}
	if s.row == nil {
		return nil, errors.New("missing")
	}
	s.row.BalanceAmount = amount
	s.row.BalanceCurrency = currency
	s.row.BalanceStatus = status
	s.row.BalanceCheckedAt = checkedAt
	copy := *s.row
	return &copy, nil
}

func (s *upstreamServiceStub) RecordUpstreamModels(_ context.Context, expected *domain.Upstream, models []string, modelErr *string) (*domain.Upstream, error) {
	if s.recordModelsFn != nil {
		return s.recordModelsFn(expected, models, modelErr)
	}
	if s.row == nil {
		return nil, errors.New("missing")
	}
	s.row.Models = append([]string(nil), models...)
	now := time.Now()
	s.row.ModelsCheckedAt = &now
	s.row.ModelsError = modelErr
	copy := *s.row
	copy.Models = append([]string(nil), s.row.Models...)
	return &copy, nil
}

func TestValidateBaseURLRejectsAmbiguousRoots(t *testing.T) {
	for _, value := range []string{
		"ftp://relay.example.com",
		"https://relay.example.com?token=secret",
		"https://relay.example.com/path#fragment",
		"https://user:pass@relay.example.com",
		"https://:443",
	} {
		require.ErrorIs(t, validateBaseURL(value), ErrInvalidInput, value)
	}
	for _, value := range []string{"http://relay.example.com", "https://relay.example.com/openai"} {
		require.NoError(t, validateBaseURL(value), value)
	}
}

func TestDeletedUpstreamCannotBeMutatedOrProbed(t *testing.T) {
	deletedAt := time.Now()
	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 9, Name: "gone", BaseURL: "https://relay.example.com", DeletedAt: &deletedAt}}
	svc := &Service{upstreams: stub}

	_, err := svc.UpdateUpstream(context.Background(), &domain.Upstream{ID: 9, Name: "new", BaseURL: "https://relay.example.com", MultiplierBP: 10000})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.ProbeUpstream(context.Background(), 9)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.RefreshUpstreamBalance(context.Background(), 9)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.SetUpstreamEnabled(context.Background(), 9, true)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestUpstreamProbeAndBalanceRejectNonPositiveIDs(t *testing.T) {
	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: "https://relay.example.com",
	}}
	svc := &Service{upstreams: stub}

	_, err := svc.ProbeUpstream(context.Background(), 0)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.ProbeUpstream(context.Background(), -1)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.RefreshUpstreamBalance(context.Background(), 0)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.RefreshUpstreamBalance(context.Background(), -1)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestSetUpstreamEnabledOnlyChangesInventoryState(t *testing.T) {
	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 7, Name: "relay", BaseURL: "https://relay.example.test", MultiplierBP: 8000, Enabled: true}}
	svc := &Service{upstreams: stub}

	updated, err := svc.SetUpstreamEnabled(context.Background(), 7, false)
	require.NoError(t, err)
	require.False(t, updated.Enabled)
	require.Equal(t, 8000, updated.MultiplierBP)
	require.Equal(t, "https://relay.example.test", updated.BaseURL)
}

func TestRefreshUpstreamBalanceAutoRequiresKey(t *testing.T) {
	checkedAt := time.Now().Add(-time.Hour)
	amount := "12.50"
	currency := "USD"
	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: "https://relay.example.com",
		BalanceStatus: domain.UpstreamBalanceFresh, BalanceAmount: &amount,
		BalanceCurrency: &currency, BalanceCheckedAt: &checkedAt,
	}}
	svc := &Service{upstreams: stub}
	result, err := svc.RefreshUpstreamBalance(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "auth", result.ErrorCode)
	require.Equal(t, domain.UpstreamBalanceUnavailable, result.Upstream.BalanceStatus)
	require.Nil(t, result.Upstream.BalanceAmount)
	require.Nil(t, result.Upstream.BalanceCurrency)
	require.Nil(t, result.Upstream.BalanceCheckedAt)
	require.False(t, result.OK)
}

func TestRefreshUpstreamBalanceAutoDetectsCommonRelayShape(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/self" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"balance":"12.34","currency":"USD"}}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.RefreshUpstreamBalance(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, domain.UpstreamBalanceFresh, result.Upstream.BalanceStatus)
	require.NotNil(t, result.Upstream.BalanceAmount)
	require.Equal(t, "12.34", *result.Upstream.BalanceAmount)
	require.NotNil(t, result.Upstream.BalanceCurrency)
	require.Equal(t, "USD", *result.Upstream.BalanceCurrency)
}

func TestTestUpstreamSendsHiUsingDiscoveredResponsesModel(t *testing.T) {
	key := "relay-key"
	var got map[string]any
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6-sol"}]}`))
			return
		}
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_test"}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.GreaterOrEqual(t, result.LatencyMS, int64(0))
	require.Equal(t, "gpt-5.6-sol", got["model"])
	require.Equal(t, "hi", got["input"])
	require.Equal(t, false, got["stream"])
	require.Equal(t, false, got["store"])
}

func TestTestUpstreamFallsBackToChatCompletions(t *testing.T) {
	key := "relay-key"
	var paths []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
			return
		}
		if r.URL.Path == "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chat_test"}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, []string{"/v1/models", "/v1/responses", "/v1/chat/completions"}, paths)
}

func TestTestUpstreamFallsBackToUnversionedModelsCatalogue(t *testing.T) {
	key := "relay-key"
	var paths []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusNotFound)
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
		case "/v1/responses":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"resp_test"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.Equal(t, []string{"/v1/models", "/models", "/v1/responses"}, paths)
}

func TestTestUpstreamFallsBackWhenResponsesNotImplemented(t *testing.T) {
	key := "relay-key"
	var paths []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
		case "/v1/responses":
			// Some relays use the standards-compliant 501 status when the
			// Responses API is not implemented.
			w.WriteHeader(http.StatusNotImplemented)
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chat_test"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.Equal(t, []string{"/v1/models", "/v1/responses", "/v1/chat/completions"}, paths)
}

func TestShouldFallbackTestIncludesNotImplemented(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
		http.StatusNotImplemented,
		http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity,
	} {
		require.Truef(t, shouldFallbackTest(status), "status %d should trigger protocol fallback", status)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		require.Falsef(t, shouldFallbackTest(status), "status %d should not trigger protocol fallback", status)
	}
}

func TestShouldFallbackTestRequestDoesNotRetryModelValidation(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		err := &upstreamHTTPError{status: status, body: []byte(`{"error":{"message":"model not found"}}`)}
		require.Falsef(t, shouldFallbackTestRequest(status, err), "status %d model errors must not retry", status)
	}
}

func TestShouldFallbackTestRequestRetriesExplicitProtocolError(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		err := &upstreamHTTPError{status: status, body: []byte(`{"error":{"message":"Responses API is not supported"}}`)}
		require.Truef(t, shouldFallbackTestRequest(status, err), "status %d protocol errors should retry", status)
	}
}

func TestShouldFallbackTestRequestDoesNotRetrySuccessfulModelErrorEnvelope(t *testing.T) {
	err := &upstreamErrorEnvelope{body: []byte(`{"error":{"message":"model unavailable"}}`)}
	require.False(t, shouldFallbackTestRequest(http.StatusOK, err))
	require.Equal(t, "model_unavailable", classifyModelValidationError(context.Background(), http.StatusOK, err))
}

func TestTestUpstreamDoesNotRetryMissingModel(t *testing.T) {
	key := "relay-key"
	var paths []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "http_error", result.ErrorCode)
	require.Equal(t, []string{"/v1/models", "/v1/responses"}, paths)
}

func TestTestUpstreamRejectsSuccessfulNonJSONResponse(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>proxy login</html>"))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "invalid_response", result.ErrorCode)
}

func TestTestUpstreamRejectsSuccessfulErrorEnvelope(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":{"message":"provider unavailable"}}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "invalid_response", result.ErrorCode)
}

func TestTestUpstreamFallsBackOnSuccessfulErrorEnvelope(t *testing.T) {
	key := "relay-key"
	var paths []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
			return
		}
		if r.URL.Path == "/v1/responses" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"error":{"message":"Responses API is not supported"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-ok","object":"chat.completion"}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.Equal(t, []string{"/v1/models", "/v1/responses", "/v1/chat/completions"}, paths)
}

func TestIsJSONObjectResponseRejectsEmptyAndScalar(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(""), []byte("null"), []byte("[]"), []byte(`"ok"`), []byte("<html>")} {
		require.Falsef(t, isJSONObjectResponse(body), "body %q must be rejected", body)
	}
	require.True(t, isJSONObjectResponse([]byte(`{"id":"ok"}`)))
	require.False(t, isJSONObjectResponse([]byte(`{"error":{"message":"failed"}}`)))
}

func TestListUpstreamModelsDeduplicatesRealCatalogue(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-a"},{"id":"model-b"}]}`))
		case "/v1/responses", "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"verified"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	oldModels := []string{"stale-model"}
	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key, Models: oldModels}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.ListUpstreamModels(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, []string{"model-a", "model-b"}, result.Models)
	require.Equal(t, 2, result.ModelsTotal)
	require.Equal(t, 2, result.ModelsChecked)
	require.Equal(t, 2, result.ModelsAvailable)
	require.Zero(t, result.ModelsFailed)
	require.True(t, result.ValidationComplete)
	require.Equal(t, result.Models, stub.row.Models)
}

func TestListUpstreamModelsKeepsOnlyModelsWithVerifiedJSONResponse(t *testing.T) {
	key := "relay-key"
	var active, maxActive int32
	var modelCRequests int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"},{"id":"model-c"}]}`))
		case "/v1/responses", "/v1/chat/completions":
			var request struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			current := atomic.AddInt32(&active, 1)
			for {
				old := atomic.LoadInt32(&maxActive)
				if current <= old || atomic.CompareAndSwapInt32(&maxActive, old, current) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			if request.Model == "model-b" && r.URL.Path == "/v1/responses" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if request.Model == "model-c" {
				atomic.AddInt32(&modelCRequests, 1)
				_, _ = w.Write([]byte(`{"error":{"message":"model unavailable"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.ListUpstreamModels(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, []string{"model-a", "model-b"}, result.Models)
	require.Equal(t, "model_unavailable", result.ErrorCode)
	require.Equal(t, 3, result.ModelsTotal)
	require.Equal(t, 3, result.ModelsChecked)
	require.Equal(t, 2, result.ModelsAvailable)
	require.Equal(t, 1, result.ModelsFailed)
	require.True(t, result.ValidationComplete)
	require.LessOrEqual(t, atomic.LoadInt32(&maxActive), int32(upstreamModelValidationConcurrency))
	require.Equal(t, int32(1), atomic.LoadInt32(&modelCRequests), "model rejection must not trigger a second protocol probe")
	require.Equal(t, []string{"model-a", "model-b"}, stub.row.Models)
	require.NotNil(t, stub.row.ModelsCheckedAt)
}

func TestListUpstreamModelsHonorsCallerDeadline(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
			return
		}
		// Keep the handler finite; the client-side deadline should still abort
		// the request well before this delayed response is written.
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err := svc.ListUpstreamModels(ctx, 1)
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "timeout", result.ErrorCode)
	require.Equal(t, 2, result.ModelsTotal)
	require.Equal(t, 2, result.ModelsChecked)
	require.Zero(t, result.ModelsAvailable)
	require.Equal(t, 2, result.ModelsFailed)
	require.False(t, result.ValidationComplete)
}

func TestTestUpstreamRejectsModelOutsideRealCatalogue(t *testing.T) {
	key := "relay-key"
	called := false
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
			return
		}
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstreamWithModel(context.Background(), 1, "model-missing")
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "model_unavailable", result.ErrorCode)
	require.False(t, called, "unsupported model must not send hi")
}

func TestRefreshUpstreamBalanceStoresFreshSnapshot(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"remaining":"42.50","currency":"USD"}}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key,
		BalanceEndpoint: endpoint.URL, BalanceMethod: http.MethodGet, BalanceAuth: "bearer",
		BalancePath: "data.remaining", BalanceCurrencyPath: "data.currency",
	}}
	svc := &Service{upstreams: stub}

	result, err := svc.RefreshUpstreamBalance(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.Equal(t, domain.UpstreamBalanceFresh, result.Upstream.BalanceStatus)
	require.NotNil(t, result.Upstream.BalanceAmount)
	require.Equal(t, "42.50", *result.Upstream.BalanceAmount)
	require.NotNil(t, result.Upstream.BalanceCurrency)
	require.Equal(t, "USD", *result.Upstream.BalanceCurrency)
	require.NotNil(t, result.Upstream.BalanceCheckedAt)
}

func TestRefreshUpstreamBalanceKeepsRecentSnapshotStaleOnFailure(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer endpoint.Close()

	checkedAt := time.Now().Add(-time.Minute)
	amount := "42.50"
	currency := "USD"
	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL,
		BalanceEndpoint: endpoint.URL, BalanceMethod: http.MethodGet, BalanceAuth: "none", BalancePath: "data.remaining",
		BalanceStatus: domain.UpstreamBalanceFresh, BalanceAmount: &amount, BalanceCurrency: &currency, BalanceCheckedAt: &checkedAt,
	}}
	svc := &Service{upstreams: stub}

	result, err := svc.RefreshUpstreamBalance(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "upstream", result.ErrorCode)
	require.Equal(t, domain.UpstreamBalanceStale, result.Upstream.BalanceStatus)
	require.NotNil(t, result.Upstream.BalanceAmount)
	require.Equal(t, amount, *result.Upstream.BalanceAmount)
	require.NotNil(t, result.Upstream.BalanceCurrency)
	require.Equal(t, currency, *result.Upstream.BalanceCurrency)
	require.NotNil(t, result.Upstream.BalanceCheckedAt)
	require.True(t, result.Upstream.BalanceCheckedAt.Equal(checkedAt))
}

func TestCreateUpstreamRejectsClearAndKeyTogether(t *testing.T) {
	key := "relay-key"
	svc := &Service{upstreams: &upstreamServiceStub{}}

	_, err := svc.CreateUpstream(context.Background(), &domain.Upstream{
		Name: "relay", BaseURL: "https://relay.example.com", MultiplierBP: 10000,
		UpstreamKey: &key, ClearUpstreamKey: true,
	})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestValidateUpstreamRejectsUnknownBalanceStatus(t *testing.T) {
	u := &domain.Upstream{
		Name: "relay", BaseURL: "https://relay.example.com", MultiplierBP: 10000,
		BalanceStatus: "invented",
	}
	require.ErrorIs(t, validateUpstream(u), ErrInvalidInput)
}

func TestProbeDropsResultWhenEndpointChangesInFlight(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()

	key := "old-key"
	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key,
	}}
	stub.recordProbeFn = func(_ *domain.Upstream, _ bool, _ int64, _ *string) (*domain.Upstream, error) {
		stub.row.BaseURL = "https://new.example.test"
		return nil, repository.ErrConflict
	}
	svc := &Service{upstreams: stub}

	result, err := svc.ProbeUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "superseded", result.ErrorCode)
	require.Equal(t, "https://new.example.test", result.Upstream.BaseURL)
	require.Zero(t, result.Upstream.RequestCount)
}

func TestProbeRequiresValidModelsPayload(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantOK      bool
		wantCode    string
	}{
		{name: "html portal", contentType: "text/html", body: "<html>login</html>", wantCode: "invalid_value"},
		{name: "empty json", contentType: "application/json", body: `{}`, wantCode: "invalid_value"},
		{name: "catalogue", contentType: "application/json", body: `{"data":[{"id":"gpt-5.6"}]}`, wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer endpoint.Close()

			stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL}}
			svc := &Service{upstreams: stub}
			result, err := svc.ProbeUpstream(context.Background(), 1)
			require.NoError(t, err)
			require.Equal(t, tc.wantOK, result.OK)
			require.Equal(t, tc.wantCode, result.ErrorCode)
		})
	}
}

func TestBalanceRefreshDropsResultWhenReaderChangesInFlight(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"remaining":"42.50"}}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL,
		BalanceEndpoint: endpoint.URL, BalanceMethod: http.MethodGet, BalanceAuth: "none", BalancePath: "data.remaining",
	}}
	stub.recordBalanceFn = func(_ *domain.Upstream, _ *string, _ *string, _ string, _ *time.Time) (*domain.Upstream, error) {
		stub.row.BalancePath = "data.available"
		return nil, repository.ErrConflict
	}
	svc := &Service{upstreams: stub}

	result, err := svc.RefreshUpstreamBalance(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "superseded", result.ErrorCode)
	require.Equal(t, "data.available", result.Upstream.BalancePath)
	require.Nil(t, result.Upstream.BalanceAmount)
}

func TestUpdateUpstreamRequiresNewOrClearedKeyWhenAddressChanges(t *testing.T) {
	key := "old-relay-key"
	amount := "42.50"
	checkedAt := time.Now().Add(-time.Minute)
	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: "https://old.example.test", UpstreamKey: &key, MultiplierBP: 10000,
		BalanceStatus: domain.UpstreamBalanceFresh, BalanceAmount: &amount, BalanceCheckedAt: &checkedAt,
	}}
	svc := &Service{upstreams: stub}

	_, err := svc.UpdateUpstream(context.Background(), &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: "https://new.example.test", MultiplierBP: 10000,
	})
	require.ErrorIs(t, err, ErrInvalidInput)

	updated, err := svc.UpdateUpstream(context.Background(), &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: "https://new.example.test", MultiplierBP: 10000, ClearUpstreamKey: true,
	})
	require.NoError(t, err)
	require.Nil(t, updated.UpstreamKey)
	require.True(t, updated.ResetTelemetry)
	require.Equal(t, domain.UpstreamBalanceUnconfigured, updated.BalanceStatus)
	require.Nil(t, updated.BalanceAmount)
	require.Nil(t, updated.BalanceCheckedAt)
}

func TestListUpstreamsRejectsInvalidQuery(t *testing.T) {
	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: "https://relay.example.com"}}
	svc := &Service{upstreams: stub}
	for _, query := range []repository.ListQuery{
		{Sort: "not-a-field"},
		{Order: "sideways"},
		{StatusList: []string{"unknown"}},
	} {
		_, _, err := svc.ListUpstreams(context.Background(), query)
		require.ErrorIs(t, err, ErrInvalidInput)
	}
}
