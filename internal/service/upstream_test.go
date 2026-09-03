// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// The legacy surface cannot express an incomplete run, matching the
	// repository: whatever it receives is published as authoritative.
	return s.recordModels(expected, models, modelErr, true)
}

func (s *upstreamServiceStub) recordModels(expected *domain.Upstream, models []string, modelErr *string, complete bool) (*domain.Upstream, error) {
	if s.recordModelsFn != nil {
		return s.recordModelsFn(expected, models, modelErr)
	}
	if s.row == nil {
		return nil, errors.New("missing")
	}
	// A nil model slice paired with an error represents an incomplete
	// catalogue/transport run; production keeps the previous verified snapshot
	// and only updates the visible error. A non-nil (possibly empty) slice
	// contributes its confirmed models.
	if !(modelErr != nil && models == nil) {
		s.row.Models = append([]string(nil), models...)
		// Only a complete run may stamp ModelsCheckedAt: routing reads that
		// timestamp as the upstream's exhaustive capability set.
		if complete {
			now := time.Now()
			s.row.ModelsCheckedAt = &now
		}
	}
	s.row.ModelsError = modelErr
	copy := *s.row
	copy.Models = append([]string(nil), s.row.Models...)
	return &copy, nil
}

func (s *upstreamServiceStub) RecordUpstreamModelCapabilities(_ context.Context, expected *domain.Upstream, models []string, modelFormats map[string][]domain.RequestFormat, modelErr *string, complete bool) (*domain.Upstream, error) {
	saved, err := s.recordModels(expected, models, modelErr, complete)
	if err != nil || models == nil {
		return saved, err
	}
	formats := cloneModelFormatSnapshot(modelFormats)
	if s.row != nil {
		s.row.ModelFormats = cloneModelFormatSnapshot(formats)
	}
	if saved != nil {
		saved.ModelFormats = formats
	}
	return saved, nil
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
		_, _ = w.Write([]byte(`{"id":"resp_test","object":"response"}`))
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
	require.Equal(t, []domain.RequestFormat{domain.FormatOpenAIResponses}, stub.row.ModelFormats["gpt-5.6-sol"])
}

func TestTestUpstreamUsesFreshProbeBudgetAfterSlowModelDiscovery(t *testing.T) {
	// Keep this regression fast while preserving the production relationship:
	// discovery and the actual model request each receive a full bounded window.
	previousTimeout := upstreamManualModelTestTimeout
	upstreamManualModelTestTimeout = 120 * time.Millisecond
	t.Cleanup(func() { upstreamManualModelTestTimeout = previousTimeout })

	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			time.Sleep(75 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"slow-catalogue-model"}]}`))
		case "/v1/responses":
			// The old implementation reused the catalogue deadline here. By the
			// time this response arrives that shared 120ms budget has expired;
			// separate phase contexts leave the probe its own 120ms window.
			time.Sleep(75 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"response-ok","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK, "a slow catalogue must not consume the model probe budget")
	require.Empty(t, result.ErrorCode)
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
		_, _ = w.Write([]byte(`{"id":"chat_test","object":"chat.completion"}`))
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
			_, _ = w.Write([]byte(`{"id":"resp_test","object":"response"}`))
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
			_, _ = w.Write([]byte(`{"id":"chat_test","object":"chat.completion"}`))
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

func TestShouldFallbackTestRequestDoesNotRetrySuccessfulProviderErrorEnvelope(t *testing.T) {
	for _, message := range []string{"provider unavailable", "quota exceeded", "authentication failed", "this model does not support tools"} {
		err := &upstreamErrorEnvelope{body: []byte(`{"error":{"message":"` + message + `"}}`)}
		require.Falsef(t, shouldFallbackTestRequest(http.StatusOK, err), "message %q must not trigger a second paid probe", message)
	}
}

func TestShouldFallbackTestRequestRetriesOnlyExplicitResponsesProtocolError(t *testing.T) {
	retryMessages := []string{
		`{"error":{"message":"Responses API is not supported"}}`,
		`{"error":{"message":"Responses endpoint not implemented"}}`,
		`{"error":{"message":"POST /v1/responses: method not allowed"}}`,
	}
	for _, body := range retryMessages {
		require.Truef(t, shouldFallbackTestRequest(http.StatusOK, &upstreamErrorEnvelope{body: []byte(body)}), "body %s must trigger protocol fallback", body)
	}
	for _, body := range []string{
		`{"error":{"message":"invalid request"}}`,
		`{"error":{"message":"model does not support this input"}}`,
		`{"error":{"message":"provider returned an unsupported feature"}}`,
	} {
		require.Falsef(t, shouldFallbackTestRequest(http.StatusOK, &upstreamErrorEnvelope{body: []byte(body)}), "body %s must not trigger protocol fallback", body)
	}
}

func TestShouldFallbackTestRequestTreatsGenericNotFoundAsMissingResponsesRoute(t *testing.T) {
	require.True(t, shouldFallbackTestRequest(http.StatusNotFound,
		&upstreamHTTPError{status: http.StatusNotFound, body: []byte(`{"error":{"message":"not found"}}`)}))
	require.True(t, shouldFallbackTestRequest(http.StatusNotFound,
		&upstreamHTTPError{status: http.StatusNotFound, body: []byte("Not Found")}))
	require.True(t, shouldFallbackTestRequest(http.StatusNotFound,
		&upstreamHTTPError{status: http.StatusNotFound, body: []byte(`{"error":{"message":"endpoint not found"}}`)}))
}

func TestTestUpstreamFallsBackOnStructuredGenericNotFound(t *testing.T) {
	key := "relay-key"
	var paths []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"chat-only-model"}]}`))
		case "/v1/responses":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"Not Found"}}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"chat-ok","object":"chat.completion"}`))
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
	require.Equal(t, []string{"/v1/models", "/v1/responses", "/v1/chat/completions"}, paths)
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
	var paths []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
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
	require.Equal(t, []string{"/v1/models", "/v1/responses"}, paths, "an unrecognized 2xx body must not trigger a second paid probe")
}

func TestShouldFallbackTestRequestDoesNotRetryInvalidSuccessfulResponse(t *testing.T) {
	require.False(t, shouldFallbackTestRequest(http.StatusOK, errInvalidUpstreamResponse))
}

func TestClassifySuccessfulErrorEnvelope(t *testing.T) {
	for _, test := range []struct {
		message string
		code    string
	}{
		{`{"error":{"message":"model unavailable"}}`, "model_unavailable"},
		{`{"error":{"message":"authentication failed"}}`, "auth"},
		{`{"error":{"message":"quota exceeded"}}`, "rate_limited"},
		{`{"error":{"message":"provider unavailable"}}`, "upstream"},
		{`{"error":{"message":"unexpected response"}}`, "invalid_response"},
	} {
		require.Equal(t, test.code, classifySuccessfulErrorEnvelope(test.message), test.message)
	}
}

func TestClassifyModelValidationErrorPreservesStrongHTTPStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		err := &upstreamHTTPError{status: status, body: []byte(`{"error":{"message":"model unavailable"}}`)}
		got := classifyModelValidationError(context.Background(), status, err)
		switch status {
		case http.StatusUnauthorized:
			require.Equal(t, "auth", got)
		case http.StatusTooManyRequests:
			require.Equal(t, "rate_limited", got)
		case http.StatusServiceUnavailable:
			require.Equal(t, "upstream", got)
		}
	}
}

func TestClassifyModelValidationErrorDoesNotGuessModelFromGenericHTTPFailure(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity} {
		err := &upstreamHTTPError{status: status, body: []byte(`{"error":{"message":"invalid request format"}}`)}
		require.Equalf(t, "http_error", classifyModelValidationError(context.Background(), status, err), "status %d", status)
	}
}

func TestClassifyModelValidationErrorMarksOnlyExplicitModelFailure(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity} {
		err := &upstreamHTTPError{status: status, body: []byte(`{"error":{"message":"model not found"}}`)}
		require.Equalf(t, "model_unavailable", classifyModelValidationError(context.Background(), status, err), "status %d", status)
	}
}

func TestTestUpstreamRejectsSuccessfulErrorEnvelope(t *testing.T) {
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
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":{"message":"provider unavailable"}}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "upstream", result.ErrorCode)
	require.Equal(t, []string{"/v1/models", "/v1/responses"}, paths, "a provider error envelope must not be retried through Chat Completions")
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
	for _, body := range [][]byte{nil, []byte(""), []byte("null"), []byte("[]"), []byte(`"ok"`), []byte("<html>"), []byte(`{"data":[]}`)} {
		require.Falsef(t, isJSONObjectResponse(body), "body %q must be rejected", body)
	}
	require.False(t, isJSONObjectResponse([]byte(`{"id":"ok"}`)))
	require.True(t, isJSONObjectResponse([]byte(`{"id":"ok","object":"response"}`)))
	require.True(t, isJSONObjectResponse([]byte(`{"choices":[]}`)))
	require.True(t, isJSONObjectResponse([]byte(`{"output":[]}`)))
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
			_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
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
	require.Equal(t, []domain.RequestFormat{domain.FormatOpenAIResponses}, stub.row.ModelFormats["model-a"])
	require.Equal(t, []domain.RequestFormat{domain.FormatOpenAIResponses}, stub.row.ModelFormats["model-b"])
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

func TestListUpstreamModelsPreservesRateLimitCauseWhenSomeModelsPass(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-ok"},{"id":"model-limited"}]}`))
		case "/v1/responses":
			var body struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if body.Model == "model-limited" {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
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
	require.Equal(t, []string{"model-ok"}, result.Models)
	require.Equal(t, "rate_limited", result.ErrorCode)
	require.Equal(t, 2, result.ModelsChecked)
	require.Equal(t, 1, result.ModelsFailed)
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
	// Probes for one upstream are serialized to avoid triggering per-credential
	// concurrency limits. With the caller deadline expiring during the first
	// request, later models remain untouched rather than being falsely counted.
	require.Equal(t, 1, result.ModelsChecked)
	require.Zero(t, result.ModelsAvailable)
	require.Equal(t, 1, result.ModelsFailed)
	require.False(t, result.ValidationComplete)
}

func TestTestUpstreamProbesExplicitModelOutsideRealCatalogue(t *testing.T) {
	key := "relay-key"
	called := false
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
			return
		}
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"manual-response","object":"response"}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.TestUpstreamWithModel(context.Background(), 1, "model-missing")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.True(t, called, "an explicit model test must use the completion route even when /models omitted the alias")
}

func TestRefreshUpstreamBalanceStoresFreshSnapshot(t *testing.T) {
	key := "Bearer Bearer relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer relay-key", r.Header.Get("Authorization"))
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

func TestRefreshUpstreamBalanceNormalizesVersionedBaseForAutoLookup(t *testing.T) {
	key := "Bearer relay-key"
	var paths []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		require.Equal(t, "Bearer relay-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/usage" {
			_, _ = w.Write([]byte(`{"remaining":"7.25","unit":"USD"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL + "/v1", UpstreamKey: &key,
	}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}

	result, err := svc.RefreshUpstreamBalance(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, "7.25", *result.Upstream.BalanceAmount)
	require.Equal(t, []string{"/v1/usage"}, paths)
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
		{name: "200 auth envelope", contentType: "application/json", body: `{"error":{"message":"invalid api key"}}`, wantCode: "auth"},
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

func TestUpdateUpstreamTreatsV1SuffixAsEquivalentAddress(t *testing.T) {
	key := "relay-key"
	amount := "42.50"
	checkedAt := time.Now().Add(-time.Minute)
	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: "https://relay.example.test/v1/", UpstreamKey: &key,
		MultiplierBP: 10000, BalanceStatus: domain.UpstreamBalanceFresh,
		BalanceAmount: &amount, BalanceCheckedAt: &checkedAt,
	}}
	svc := &Service{upstreams: stub}

	updated, err := svc.UpdateUpstream(context.Background(), &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: "https://relay.example.test", MultiplierBP: 10000,
	})
	require.NoError(t, err)
	require.Equal(t, "https://relay.example.test", updated.BaseURL)
	require.False(t, updated.ResetTelemetry, "an equivalent /v1 spelling must not reset telemetry")
	require.Equal(t, domain.UpstreamBalanceFresh, updated.BalanceStatus)
	require.Equal(t, &amount, updated.BalanceAmount)
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

func TestUpstreamValidationMethodsHandleNilService(t *testing.T) {
	var svc *Service
	_, err := svc.CreateUpstreamWithModelValidation(context.Background(), &domain.Upstream{})
	require.ErrorIs(t, err, errUpstreamStoreUnavailable)
	_, err = svc.ListUpstreamModels(context.Background(), 1)
	require.ErrorIs(t, err, errUpstreamStoreUnavailable)
	_, err = svc.ValidateAllUpstreams(context.Background())
	require.ErrorIs(t, err, errUpstreamStoreUnavailable)
}

func TestUpstreamModelValidationAcceptsV1SuffixWithoutDuplicatingPath(t *testing.T) {
	key := "relay-key"
	var paths []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL + "/v1", UpstreamKey: &key}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.ListUpstreamModels(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, []string{"model-a"}, result.Models)
	require.Equal(t, []string{"/v1/models", "/v1/responses"}, paths)
}

func TestListUpstreamModelsClearsStaleSnapshotAfterCompleteFailures(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer endpoint.Close()

	oldModels := []string{"stale-model"}
	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key, Models: oldModels}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.ListUpstreamModels(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "auth", result.ErrorCode)
	require.True(t, result.ValidationComplete)
	require.Empty(t, stub.row.Models, "a complete failed validation must not leave stale routable models")
	require.NotNil(t, stub.row.ModelsCheckedAt)
}

func TestListUpstreamModelsEmptyCatalogueIsCompletedAndClearsSnapshot(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key, Models: []string{"stale-model"}}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.ListUpstreamModels(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, "model_unavailable", result.ErrorCode)
	require.True(t, result.ValidationComplete)
	require.Empty(t, result.Models)
	require.Empty(t, stub.row.Models)
	require.NotNil(t, stub.row.ModelsCheckedAt)
}

func TestListUpstreamModelsIncompleteRunKeepsPreviousSnapshot(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
			return
		}
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer endpoint.Close()

	oldModels := []string{"stale-model"}
	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key, Models: oldModels}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err := svc.ListUpstreamModels(ctx, 1)
	require.NoError(t, err)
	require.False(t, result.ValidationComplete)
	require.Equal(t, oldModels, stub.row.Models, "incomplete validation must preserve the last known snapshot")
}

func TestValidateAllUpstreamsChecksEachRowAndRecordsHealth(t *testing.T) {
	var requests int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			if strings.Contains(r.Header.Get("Authorization"), "bad") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	goodKey, badKey := "good", "bad"
	stub := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "good", BaseURL: endpoint.URL, UpstreamKey: &goodKey}}
	// The lightweight stub stores one row; use a small custom store below to
	// exercise the service-level all-upstreams fan-out without a database.
	store := &multiUpstreamServiceStub{rows: []*domain.Upstream{
		{ID: 1, Name: "good", BaseURL: endpoint.URL, UpstreamKey: &goodKey, Enabled: true},
		{ID: 2, Name: "bad", BaseURL: endpoint.URL, UpstreamKey: &badKey, Enabled: false},
	}}
	_ = stub
	svc := &Service{upstreams: store, upstreamHTTPClient: endpoint.Client()}
	summary, err := svc.ValidateAllUpstreams(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, summary.Total)
	// The unauthorized row cannot read a model catalogue, so its capability
	// validation is incomplete even though the worker returned an auth error.
	require.Equal(t, 1, summary.Completed)
	require.Equal(t, 1, summary.Passed)
	require.Zero(t, summary.Failed)
	require.Len(t, summary.Items, 2)
	require.True(t, summary.Items[0].OK)
	require.False(t, summary.Items[1].OK)
	require.Equal(t, "auth", summary.Items[1].ErrorCode)
	require.GreaterOrEqual(t, atomic.LoadInt32(&requests), int32(3))
}

func TestValidateAllUpstreamsRejectsConcurrentRun(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
			_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()
	key := "key"
	store := &multiUpstreamServiceStub{rows: []*domain.Upstream{{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key}}}
	svc := &Service{upstreams: store, upstreamHTTPClient: endpoint.Client()}
	firstDone := make(chan error, 1)
	go func() {
		_, err := svc.ValidateAllUpstreams(context.Background())
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first validation did not reach model request")
	}
	_, err := svc.ValidateAllUpstreams(context.Background())
	require.ErrorIs(t, err, ErrConflict)
	close(release)
	require.NoError(t, <-firstDone)
}

func TestValidateAllUpstreamsCancellationDoesNotCountUnfinishedRows(t *testing.T) {
	rows := make([]*domain.Upstream, 6)
	for i := range rows {
		rows[i] = &domain.Upstream{ID: int64(i + 1), Name: "relay", BaseURL: "https://relay.example.test"}
	}
	store := &multiUpstreamServiceStub{rows: rows}
	svc := &Service{upstreams: store}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary, err := svc.ValidateAllUpstreams(ctx)
	require.NoError(t, err)
	require.Equal(t, len(rows), summary.Total)
	for i, item := range summary.Items {
		if item.Attempted {
			require.False(t, item.ValidationComplete, "canceled row %d must not be complete", i)
			continue
		}
		require.Equal(t, "canceled", item.ErrorCode, "untouched row %d must be marked canceled", i)
	}
	require.Zero(t, summary.Completed, "canceled rows must not be counted as completed")
	require.Zero(t, summary.Passed)
	require.Zero(t, summary.Failed)
	require.False(t, summary.Items[0].Attempted, "a pre-canceled request must not start any probe")
}

func TestValidateAllUpstreamsMarksDeadlineRowsAsTimedOut(t *testing.T) {
	rows := []*domain.Upstream{{ID: 1, Name: "relay", BaseURL: "https://relay.example.test"}}
	store := &contextRejectingSnapshotStub{multiUpstreamServiceStub: &multiUpstreamServiceStub{rows: rows}}
	svc := &Service{upstreams: store}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	summary, err := svc.ValidateAllUpstreams(ctx)
	require.NoError(t, err)
	require.Len(t, summary.Items, 1)
	require.False(t, summary.Items[0].Attempted)
	require.Equal(t, "timeout", summary.Items[0].ErrorCode)
	require.Zero(t, summary.Completed)
}

func TestValidateAllUpstreamsPreCanceledSnapshotUsesBoundedDetachedRead(t *testing.T) {
	rows := []*domain.Upstream{{ID: 1, Name: "relay", BaseURL: "https://relay.example.test"}}
	store := &contextRejectingSnapshotStub{multiUpstreamServiceStub: &multiUpstreamServiceStub{rows: rows}}
	svc := &Service{upstreams: store}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary, err := svc.ValidateAllUpstreams(ctx)
	require.NoError(t, err)
	require.Len(t, summary.Items, 1)
	require.Equal(t, "canceled", summary.Items[0].ErrorCode)
	require.Zero(t, summary.Completed)
}

func TestSummarizeUpstreamValidationCountsOnlyCompleteCapabilityChecks(t *testing.T) {
	items := []UpstreamValidationItem{
		{Attempted: true, ValidationComplete: true, OK: true},
		{Attempted: true, ValidationComplete: true, OK: false, ErrorCode: "auth"},
		{Attempted: true, ValidationComplete: false, OK: false, ErrorCode: "timeout"},
		{Attempted: false, ValidationComplete: true, OK: true, ErrorCode: "canceled"},
	}

	summary := summarizeUpstreamValidation(items, time.Now())

	require.Equal(t, len(items), summary.Total)
	require.Equal(t, 2, summary.Completed)
	require.Equal(t, 1, summary.Passed)
	require.Equal(t, 1, summary.Failed)
	require.Equal(t, summary.Completed, summary.Passed+summary.Failed)
}

func TestValidateAllUpstreamsLoadsLargeInventoryWithoutOffsetPaging(t *testing.T) {
	rows := make([]*domain.Upstream, 250)
	for i := range rows {
		rows[i] = &domain.Upstream{ID: int64(i + 1), Name: "relay", BaseURL: "https://relay.example.test"}
	}
	store := &multiUpstreamServiceStub{rows: rows}
	svc := &Service{upstreams: store}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary, err := svc.ValidateAllUpstreams(ctx)
	require.NoError(t, err)
	require.Equal(t, len(rows), summary.Total)
	require.Len(t, summary.Items, len(rows))
	// The compatibility path may need two reads (count page + full set), but
	// both are anchored at offset zero. A later page must never be fetched with
	// OFFSET because concurrent creates/deletes could move rows between pages.
	require.GreaterOrEqual(t, len(store.listQueries), 2)
	for _, query := range store.listQueries {
		require.Zero(t, query.Offset)
	}
	require.Equal(t, len(rows), store.listQueries[len(store.listQueries)-1].Limit)
	for i, item := range summary.Items {
		require.Equal(t, int64(i+1), item.Upstream.ID)
		require.Equal(t, "canceled", item.ErrorCode)
	}
}

func TestLoadUpstreamValidationSnapshotRejectsOversizedCompatibilityInventory(t *testing.T) {
	store := &multiUpstreamServiceStub{reportedTotal: upstreamValidationSnapshotMax + 1}
	_, err := loadUpstreamValidationSnapshot(context.Background(), store)
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Len(t, store.listQueries, 1, "oversized inventory must not trigger an unbounded second read")
}

func TestLoadUpstreamValidationSnapshotRejectsShortCompatibilityRead(t *testing.T) {
	rows := make([]*domain.Upstream, upstreamValidationBatchLimit+1)
	for i := range rows {
		rows[i] = &domain.Upstream{ID: int64(i + 1)}
	}
	store := &multiUpstreamServiceStub{rows: rows, hardLimit: upstreamValidationBatchLimit}
	_, err := loadUpstreamValidationSnapshot(context.Background(), store)
	require.ErrorIs(t, err, ErrConflict)
	require.Len(t, store.listQueries, 2)
}

func TestNormalizeUpstreamKeyAcceptsCopiedBearerHeader(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{in: "secret", want: "secret"},
		{in: "  Bearer secret  ", want: "secret"},
		{in: "bEaReR   secret", want: "secret"},
		{in: "Bearer Bearer secret", want: "secret"},
		{in: "Bearer", want: "Bearer"},
	} {
		require.Equal(t, tc.want, normalizeUpstreamKey(tc.in), tc.in)
	}
}

func TestParseUpstreamModelsDoesNotSilentlyTruncateCatalogue(t *testing.T) {
	entries := make([]map[string]string, 201)
	for i := range entries {
		entries[i] = map[string]string{"id": fmt.Sprintf("model-%03d", i)}
	}
	body, err := json.Marshal(map[string]any{"data": entries})
	require.NoError(t, err)

	models, recognized := parseUpstreamModelsPayload(body)
	require.True(t, recognized)
	require.Len(t, models, len(entries))
	require.Equal(t, "model-200", models[len(models)-1])
}

func TestParseUpstreamModelsRejectsOversizedCatalogue(t *testing.T) {
	entries := make([]map[string]string, upstreamModelCatalogueMax+1)
	for i := range entries {
		entries[i] = map[string]string{"id": fmt.Sprintf("model-%04d", i)}
	}
	body, err := json.Marshal(map[string]any{"data": entries})
	require.NoError(t, err)

	models, recognized := parseUpstreamModelsPayload(body)
	require.False(t, recognized)
	require.Nil(t, models)
}

func TestParseUpstreamModelsRejectsMalformedNonEmptyCatalogue(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"data":[{"object":"model"}]}`),
		[]byte(`{"models":[{"id":123}]}`),
		[]byte(`{"data":[{"id":"valid"},{}]}`),
		[]byte(`{"data":[{"id":"page-1"}],"has_more":true,"last_id":"page-1"}`),
		[]byte(`{"data":[{"id":"page-1"}],"next_cursor":"page-2"}`),
	} {
		models, recognized := parseUpstreamModelsPayload(body)
		require.False(t, recognized, "catalogue %s must not be treated as an empty/partial list", body)
		require.Nil(t, models)
	}
}

func TestNormalizeUpstreamValidationSnapshotRejectsDuplicateOrInvalidRows(t *testing.T) {
	for _, rows := range [][]*domain.Upstream{
		{{ID: 1}, {ID: 1}},
		{{ID: 0}},
		{nil},
	} {
		_, err := normalizeUpstreamSnapshotStrict(rows)
		require.Error(t, err)
	}
}

type multiUpstreamServiceStub struct {
	rows          []*domain.Upstream
	listQueries   []repository.ListQuery
	reportedTotal int
	hardLimit     int
}

// contextRejectingSnapshotStub mirrors the production repository's behavior:
// a ListAll query observes an already-canceled request context and returns it
// immediately. ValidateAll detaches only the initial read so it can still
// report every untouched row as canceled.
type contextRejectingSnapshotStub struct {
	*multiUpstreamServiceStub
}

func (s *contextRejectingSnapshotStub) ListAllUpstreams(ctx context.Context) ([]*domain.Upstream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows := make([]*domain.Upstream, 0, len(s.rows))
	for _, row := range s.rows {
		copy := *row
		rows = append(rows, &copy)
	}
	return rows, nil
}

func (s *multiUpstreamServiceStub) CreateUpstream(_ context.Context, in *domain.Upstream) (*domain.Upstream, error) {
	return in, nil
}
func (s *multiUpstreamServiceStub) GetUpstream(_ context.Context, id int64) (*domain.Upstream, error) {
	for _, row := range s.rows {
		if row.ID == id {
			copy := *row
			if row.UpstreamKey != nil {
				key := *row.UpstreamKey
				copy.UpstreamKey = &key
			}
			return &copy, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (s *multiUpstreamServiceStub) ListUpstreams(_ context.Context, q repository.ListQuery) ([]*domain.Upstream, int64, error) {
	s.listQueries = append(s.listQueries, q)
	ordered := append([]*domain.Upstream(nil), s.rows...)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j].ID < ordered[j-1].ID; j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	total := int64(len(ordered))
	if s.reportedTotal > 0 {
		total = int64(s.reportedTotal)
	}
	start := q.Offset
	if start < 0 {
		start = 0
	}
	if start >= len(ordered) {
		return []*domain.Upstream{}, total, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if s.hardLimit > 0 && limit > s.hardLimit {
		limit = s.hardLimit
	}
	end := start + limit
	if end > len(ordered) {
		end = len(ordered)
	}
	out := make([]*domain.Upstream, 0, end-start)
	for _, row := range ordered[start:end] {
		copy := *row
		out = append(out, &copy)
	}
	return out, total, nil
}
func (s *multiUpstreamServiceStub) UpdateUpstream(_ context.Context, in *domain.Upstream) (*domain.Upstream, error) {
	return in, nil
}
func (s *multiUpstreamServiceStub) SetUpstreamEnabled(_ context.Context, id int64, enabled bool) (*domain.Upstream, error) {
	row, err := s.GetUpstream(context.Background(), id)
	if err != nil {
		return nil, err
	}
	row.Enabled = enabled
	return row, nil
}
func (s *multiUpstreamServiceStub) DeleteUpstream(_ context.Context, _ int64) error { return nil }
func (s *multiUpstreamServiceStub) RecordUpstreamProbe(_ context.Context, expected *domain.Upstream, success bool, latencyMS int64, probeErr *string) (*domain.Upstream, error) {
	for _, row := range s.rows {
		if row.ID == expected.ID {
			row.RequestCount++
			if success {
				row.SuccessCount++
			} else {
				row.FailureCount++
			}
			row.LatencyTotalMS += latencyMS
			row.LastError = probeErr
			copy := *row
			return &copy, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (s *multiUpstreamServiceStub) RecordUpstreamBalance(context.Context, *domain.Upstream, *string, *string, string, *time.Time) (*domain.Upstream, error) {
	return nil, errors.New("not implemented")
}
func (s *multiUpstreamServiceStub) RecordUpstreamModels(_ context.Context, expected *domain.Upstream, models []string, modelErr *string) (*domain.Upstream, error) {
	// Legacy surface: publishes whatever it receives as authoritative.
	return s.recordModels(expected, models, modelErr, true)
}

func (s *multiUpstreamServiceStub) recordModels(expected *domain.Upstream, models []string, modelErr *string, complete bool) (*domain.Upstream, error) {
	for _, row := range s.rows {
		if row.ID == expected.ID {
			if !(modelErr != nil && models == nil) {
				row.Models = append([]string(nil), models...)
				// Only a complete run may claim the snapshot is exhaustive.
				if complete {
					now := time.Now()
					row.ModelsCheckedAt = &now
				}
			}
			row.ModelsError = modelErr
			copy := *row
			copy.Models = append([]string(nil), row.Models...)
			return &copy, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (s *multiUpstreamServiceStub) RecordUpstreamModelCapabilities(_ context.Context, expected *domain.Upstream, models []string, modelFormats map[string][]domain.RequestFormat, modelErr *string, complete bool) (*domain.Upstream, error) {
	saved, err := s.recordModels(expected, models, modelErr, complete)
	if err != nil || models == nil {
		return saved, err
	}
	formats := cloneModelFormatSnapshot(modelFormats)
	for _, row := range s.rows {
		if row.ID == expected.ID {
			row.ModelFormats = cloneModelFormatSnapshot(formats)
			break
		}
	}
	if saved != nil {
		saved.ModelFormats = formats
	}
	return saved, nil
}
