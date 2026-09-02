// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// atomicUpdateStore records the candidate passed to UpdateUpstream while
// inheriting the rest of the lightweight upstream store behavior.
type atomicUpdateStore struct {
	*upstreamServiceStub
	updated *domain.Upstream
}

func (s *atomicUpdateStore) UpdateUpstream(_ context.Context, in *domain.Upstream) (*domain.Upstream, error) {
	copy := *in
	copy.Models = append([]string(nil), in.Models...)
	s.updated = &copy
	s.row = &copy
	return &copy, nil
}

func TestUpdateUpstreamWithModelValidationRejectsBeforeWrite(t *testing.T) {
	oldKey := "old-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	store := &atomicUpdateStore{upstreamServiceStub: &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &oldKey,
		MultiplierBP: 10000, UpdatedAt: time.Now(),
	}}}
	svc := &Service{upstreams: store, upstreamHTTPClient: endpoint.Client()}

	newKey := "new-key"
	_, err := svc.UpdateUpstreamWithModelValidation(context.Background(), &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &newKey,
		MultiplierBP: 10000,
	})

	var validationErr *UpstreamModelValidationError
	require.ErrorAs(t, err, &validationErr)
	require.Equal(t, "auth", validationErr.Code)
	require.Nil(t, store.updated, "a failed capability check must not write the update")
	require.Equal(t, oldKey, *store.row.UpstreamKey)
}

func TestUpdateUpstreamWithModelValidationPersistsVerifiedSnapshot(t *testing.T) {
	oldKey := "old-key"
	var responses int
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			responses++
			var body struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "model-a", body.Model)
			_, _ = w.Write([]byte(`{"id":"resp-1","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	updatedAt := time.Now().Add(-time.Minute)
	store := &atomicUpdateStore{upstreamServiceStub: &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &oldKey,
		MultiplierBP: 10000, UpdatedAt: updatedAt,
	}}}
	svc := &Service{upstreams: store, upstreamHTTPClient: endpoint.Client()}

	newKey := "new-key"
	updated, err := svc.UpdateUpstreamWithModelValidation(context.Background(), &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &newKey,
		MultiplierBP: 10000,
	})

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.NotNil(t, store.updated)
	require.Equal(t, []string{"model-a"}, store.updated.Models)
	require.NotNil(t, store.updated.ModelsCheckedAt)
	require.True(t, store.updated.ResetTelemetry)
	require.NotNil(t, store.updated.ExpectedUpdatedAt, "legacy clients receive an optimistic revision guard")
	require.Equal(t, updatedAt, *store.updated.ExpectedUpdatedAt)
	require.Equal(t, 1, responses, "one model must receive one real capability request")
}

func TestUpdateUpstreamWithoutModelChangeKeepsLegacyWritePath(t *testing.T) {
	key := "same-key"
	store := &atomicUpdateStore{upstreamServiceStub: &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: "https://relay.example.test", UpstreamKey: &key,
		MultiplierBP: 10000, UpdatedAt: time.Now(), Models: []string{"model-a"},
	}}}
	svc := &Service{upstreams: store}

	updated, err := svc.UpdateUpstreamWithModelValidation(context.Background(), &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: store.row.BaseURL, MultiplierBP: 9000,
	})

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 9000, store.updated.MultiplierBP)
	require.Nil(t, store.updated.ModelsCheckedAt)
	require.Nil(t, store.updated.ExpectedUpdatedAt, "legacy non-connection edits remain unversioned")
	require.Equal(t, []string(nil), store.updated.Models)
}

func TestValidateModelCatalogueStopsAfterFatalRelayError(t *testing.T) {
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer endpoint.Close()

	models := make([]string, 100)
	for i := range models {
		models[i] = "model-" + string(rune('a'+i%26))
	}
	result := validateModelCatalogue(context.Background(), endpoint.Client(), endpoint.URL, "key", models)

	require.False(t, result.ValidationComplete)
	require.Equal(t, "auth", result.ErrorCode)
	require.Less(t, result.ModelsChecked, len(models), "a fatal relay error must stop the remaining catalogue probes")
	require.LessOrEqual(t, requests.Load(), int32(upstreamModelValidationConcurrency*2), "only the current bounded worker batch may race with cancellation")
}

func TestValidateModelCatalogueDoesNotStopOnModelRateLimit(t *testing.T) {
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
		var body struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		if body.Model == "model-0" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"model rate limit"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
	}))
	defer endpoint.Close()

	models := []string{"model-0", "model-1", "model-2", "model-3", "model-4", "model-5"}
	result := validateModelCatalogue(context.Background(), endpoint.Client(), endpoint.URL, "key", models)

	// A 429 can apply to one model only. The validator must still check the
	// remaining catalogue and publish its verified subset in provider order, but
	// the snapshot stays retryable so a previous manual result is not erased.
	require.False(t, result.ValidationComplete)
	require.Equal(t, len(models), result.ModelsChecked)
	require.Equal(t, models[1:], result.Models)
	require.Equal(t, "rate_limited", result.ErrorCode)
	// A response status is never replayed; every catalogue entry is probed once.
	require.Equal(t, int32(len(models)), requests.Load())
}

func TestValidateModelCatalogueAvoidsConcurrencyOnlyFalseNegatives(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		// This relay allows one request per credential. A burst is reported as a
		// transient upstream failure even though the same model succeeds when
		// tested alone, which mirrors the batch/manual discrepancy.
		if current > 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"provider busy"}}`))
			return
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
	}))
	defer endpoint.Close()

	models := []string{"model-a", "model-b", "model-c", "model-d"}
	result := validateModelCatalogue(context.Background(), endpoint.Client(), endpoint.URL, "key", models)

	require.True(t, result.ValidationComplete)
	require.True(t, result.OK)
	require.Equal(t, models, result.Models)
	require.Equal(t, len(models), result.ModelsChecked)
	require.Zero(t, result.ModelsFailed)
	require.Equal(t, int32(1), maxActive.Load(), "batch validation must not burst requests for one upstream credential")
}

type validationDeadlineTransport struct{}

func (validationDeadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, ok := req.Context().Deadline()
	if !ok || time.Until(deadline) < 10*time.Second {
		return nil, context.DeadlineExceeded
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"verified","object":"response"}`)),
		Request:    req,
	}, nil
}

func TestValidateModelCatalogueUsesManualProbeBudget(t *testing.T) {
	result := validateModelCatalogue(context.Background(), &http.Client{Transport: validationDeadlineTransport{}}, "https://relay.example.test", "key", []string{"model-a"})

	require.True(t, result.ValidationComplete)
	require.True(t, result.OK)
	require.Equal(t, []string{"model-a"}, result.Models)
	require.Equal(t, 1, result.ModelsChecked)
	require.Zero(t, result.ModelsFailed)
}

func TestModelValidationTimeoutScalesWithCatalogueSize(t *testing.T) {
	require.Equal(t, 30*time.Second, modelValidationTimeoutForCount(1))
	require.Equal(t, 12*time.Minute+time.Second, modelValidationTimeoutForCount(60))
	require.Equal(t, 15*time.Minute, modelValidationTimeoutForCount(5000))
}

func TestValidateModelCatalogueKeepsTransientAllFailureIncomplete(t *testing.T) {
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"provider temporarily unavailable"}}`))
	}))
	defer endpoint.Close()

	models := []string{"model-0", "model-1", "model-2", "model-3", "model-4"}
	result := validateModelCatalogue(context.Background(), endpoint.Client(), endpoint.URL, "key", models)

	// All requests completed, but a transient outage is not proof that the
	// provider has no models. Treat the run as incomplete so persistence keeps
	// the last known-good snapshot for routing.
	require.False(t, result.ValidationComplete)
	require.Equal(t, "upstream", result.ErrorCode)
	require.Equal(t, len(models), result.ModelsChecked)
	// The transient run stays incomplete without replaying already received
	// requests; persistence retains the previous snapshot for a later run.
	require.Equal(t, int32(len(models)), requests.Load())
}

func TestListUpstreamModelsKeepsSnapshotWhenTransientFailureHidesAllModels(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-missing"},{"id":"model-limited"}]}`))
		case "/v1/responses":
			var body struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if body.Model == "model-missing" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	oldModels := []string{"previously-verified"}
	store := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key, Models: oldModels,
	}}
	svc := &Service{upstreams: store, upstreamHTTPClient: endpoint.Client()}

	result, err := svc.ListUpstreamModels(context.Background(), 1)
	require.NoError(t, err)
	// The endpoint was transiently rate-limited, but the previously verified
	// snapshot remains routable until a definitive authentication/model result
	// replaces it.
	require.True(t, result.OK)
	require.False(t, result.ValidationComplete)
	require.Equal(t, "rate_limited", result.ErrorCode)
	require.Equal(t, oldModels, store.row.Models, "a transient failure must not erase the last verified route")
}

func TestValidateModelCatalogueCallerCancellationWinsOverAuthResponse(t *testing.T) {
	started := make(chan struct{})
	var cancel context.CancelFunc
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			close(started)
			// Make the caller cancellation race with an otherwise valid auth
			// response. The final diagnostic must describe the canceled request,
			// not whichever worker happened to classify the 401 first.
			cancel()
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	ctx, cancelRequest := context.WithCancel(context.Background())
	cancel = cancelRequest
	done := make(chan upstreamModelValidation, 1)
	go func() {
		done <- validateModelCatalogue(ctx, endpoint.Client(), endpoint.URL, "key", []string{"model-a"})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("validation did not reach the model request")
	}
	result := <-done
	require.False(t, result.ValidationComplete)
	require.Equal(t, "canceled", result.ErrorCode)
}

func TestValidationCancellationWinsWhenHTTPErrorArrivesFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	httpErr := &upstreamHTTPError{status: http.StatusUnauthorized, body: []byte(`{"error":{"message":"invalid api key"}}`)}

	require.Equal(t, "canceled", classifyUpstreamTestError(ctx, http.StatusUnauthorized, httpErr))
	require.Equal(t, "canceled", classifyModelValidationError(ctx, http.StatusUnauthorized, httpErr))
}

func TestCompletedHTTPResponseWinsOverRacingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The response body has already been consumed successfully. A canceled
	// caller at this exact boundary must retain the usable result rather than
	// turning it into the misleading timeout/canceled category.
	require.Empty(t, classifyUpstreamTestError(ctx, http.StatusOK, nil))
	require.Empty(t, classifyModelValidationError(ctx, http.StatusOK, nil))
}

func TestValidateModelCatalogueUsesInternalDeadlineForCompletion(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			// Keep the request open until the bounded validation context is
			// canceled. The caller context remains live, isolating the internal
			// deadline from an outer request deadline.
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
			_, _ = w.Write([]byte(`{"id":"late","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	result := validateModelCatalogueWithTimeout(
		context.Background(), endpoint.Client(), endpoint.URL, "key", []string{"model-a"}, 20*time.Millisecond,
	)

	require.False(t, result.ValidationComplete, "an internal timeout cannot publish a complete snapshot")
	require.Equal(t, "timeout", result.ErrorCode)
	require.Equal(t, 1, result.ModelsChecked)
}
