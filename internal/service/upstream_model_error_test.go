// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Relays commonly answer a model-scoped 401/403 with wording that differs
// from "model unavailable". The validator must classify these as a
// model-specific failure so it continues probing the rest of the catalogue.
func TestModelScopedAuthFailureRecognizesCommonEntitlementMessages(t *testing.T) {
	for _, message := range []string{
		"model is not available for this account",
		"model not available with this API key",
		"model is not enabled for this project",
		"this account is not entitled to use the model",
		"no access to model gpt-x",
		"model permission denied",
	} {
		err := &upstreamHTTPError{
			status: http.StatusForbidden,
			body:   []byte(`{"error":{"message":"` + message + `"}}`),
		}
		require.Truef(t, isModelScopedAuthFailure(err), "message %q should be model-scoped", message)
		require.Equal(t, "auth", classifyModelValidationError(context.Background(), http.StatusForbidden, err), message)
	}
}

// Generic account-level authorization failures remain fatal. A broad
// substring match must not turn an invalid key into a model-only warning.
func TestModelScopedAuthFailureDoesNotDowngradeAccountAuth(t *testing.T) {
	for _, message := range []string{
		"invalid api key",
		"authentication failed",
		"forbidden",
		"credential revoked",
	} {
		err := &upstreamHTTPError{
			status: http.StatusUnauthorized,
			body:   []byte(`{"error":{"message":"` + message + `"}}`),
		}
		require.Falsef(t, isModelScopedAuthFailure(err), "message %q should remain account-scoped", message)
	}
}

// A complete pass over the catalogue can still contain a transient per-model
// failure. Keep the previous model in the routable snapshot so one 429/5xx
// does not make a manually verified model disappear from the group editor.
func TestListUpstreamModelsRetainsOldModelOnTransientFailure(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-old"},{"id":"model-ok"}]}`))
		case "/v1/responses":
			var body struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if body.Model == "model-old" {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"resp-ok","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key,
		Models: []string{"model-old"},
	}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}

	result, err := svc.ListUpstreamModels(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, "rate_limited", result.ErrorCode)
	require.False(t, result.ValidationComplete, "transient per-model failure should keep this snapshot retryable")
	require.Equal(t, []string{"model-old", "model-ok"}, result.Models)
	require.Equal(t, result.Models, stub.row.Models)
}
