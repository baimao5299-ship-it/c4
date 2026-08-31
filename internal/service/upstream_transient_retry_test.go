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

	"github.com/stretchr/testify/require"
)

// A model may be manually usable while the first automated probe hits a
// short-lived rate limit. The bounded retry should publish the model when the
// immediate second attempt succeeds.
func TestValidateModelCatalogueRetriesTransientModelProbe(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		var body struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"temporary rate limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"recovered","object":"response"}`))
	}))
	defer server.Close()

	result := validateModelCatalogue(context.Background(), server.Client(), server.URL, "relay-key", []string{"manual-model"})
	require.True(t, result.ValidationComplete)
	require.True(t, result.OK)
	require.Equal(t, []string{"manual-model"}, result.Models)
	require.Zero(t, result.ModelsFailed)
	require.Equal(t, int32(2), requests.Load())
}

func TestShouldRetryTransientModelProbeSkipsDefinitiveErrors(t *testing.T) {
	require.True(t, shouldRetryTransientModelProbe(http.StatusTooManyRequests, &upstreamHTTPError{status: http.StatusTooManyRequests}))
	require.True(t, shouldRetryTransientModelProbe(http.StatusServiceUnavailable, &upstreamHTTPError{status: http.StatusServiceUnavailable}))
	require.False(t, shouldRetryTransientModelProbe(http.StatusUnauthorized, &upstreamHTTPError{status: http.StatusUnauthorized}))
	require.False(t, shouldRetryTransientModelProbe(http.StatusOK, errInvalidUpstreamResponse))
	require.False(t, shouldRetryTransientModelProbe(0, context.DeadlineExceeded))
}
