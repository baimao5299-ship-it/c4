// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	revision := store.row.UpdatedAt
	svc := &Service{upstreams: store}

	updated, err := svc.UpdateUpstreamWithModelValidation(context.Background(), &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: store.row.BaseURL, MultiplierBP: 9000,
	})

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 9000, store.updated.MultiplierBP)
	require.Nil(t, store.updated.ModelsCheckedAt)
	require.NotNil(t, store.updated.ExpectedUpdatedAt, "legacy edits receive an optimistic revision guard")
	require.Equal(t, revision, *store.updated.ExpectedUpdatedAt)
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
