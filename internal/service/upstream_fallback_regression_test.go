// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShouldFallbackTestRequestDoesNotRetryStructured404Failures(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "quota", body: `{"error":{"message":"quota exceeded"}}`},
		{name: "quota code", body: `{"error":{"type":"insufficient_quota"}}`},
		{name: "quota code variant", body: `{"error":{"code":"quota_exceeded"}}`},
		{name: "auth", body: `{"error":{"message":"invalid api key"}}`},
		{name: "auth code", body: `{"error":{"code":"invalid_api_key"}}`},
		{name: "permission code", body: `{"error":{"code":"permission_denied"}}`},
		{name: "provider", body: `{"error":{"message":"provider unavailable"}}`},
		{name: "provider code", body: `{"error":{"code":"provider_error"}}`},
		{name: "model", body: `{"error":{"message":"model not found"}}`},
		{name: "model code variant", body: `{"error":{"code":"model-not-found"}}`},
		{name: "responses unsupported", body: `{"error":{"message":"Responses API not supported"}}`, want: true},
		{name: "route missing", body: `{"error":{"message":"endpoint not found"}}`, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &upstreamHTTPError{status: http.StatusNotFound, body: []byte(tc.body)}
			require.Equal(t, tc.want, shouldFallbackTestRequest(http.StatusNotFound, err))
		})
	}
}

func TestFetchAdvertisedModelsDoesNotProbeCompatibilityPathForStructured404Failure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer server.Close()

	models, code := fetchAdvertisedModels(context.Background(), server.Client(), server.URL, "key")
	require.Nil(t, models)
	require.NotEmpty(t, code)
	require.Equal(t, int32(1), requests.Load(), "a structured non-route 404 must not trigger /models retry")
}

func TestFetchAdvertisedModelsStillProbesCompatibilityPathForRoute404(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"endpoint not found"}}`))
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	models, code := fetchAdvertisedModels(context.Background(), server.Client(), server.URL, "key")
	require.Equal(t, []string{"model-a"}, models)
	require.Empty(t, code)
	require.Equal(t, int32(2), requests.Load())
}
