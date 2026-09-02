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

	"github.com/is7qin/c3api/internal/domain"
)

// A model catalogue can contain entries for which the credential has no
// entitlement.  That 401/403 is model-scoped when /v1/models itself succeeded;
// it must not stop the validator before it reaches usable entries.
func TestValidateModelCatalogueDoesNotStopAfterModelScopedAuthError(t *testing.T) {
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
		if body.Model == "restricted-model" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"model access denied"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-ok","object":"response"}`))
	}))
	defer endpoint.Close()

	result := validateModelCatalogue(context.Background(), endpoint.Client(), endpoint.URL, "key", []string{"restricted-model", "usable-model"})

	require.True(t, result.ValidationComplete)
	require.True(t, result.OK)
	require.Equal(t, []string{"usable-model"}, result.Models)
	require.Equal(t, 2, result.ModelsChecked)
	require.Equal(t, 1, result.ModelsFailed)
	require.Equal(t, "auth", result.ErrorCode)
	require.Equal(t, int32(2), requests.Load())
}

func TestModelScopedAuthMessageVariantsDoNotStopValidation(t *testing.T) {
	for _, message := range []string{
		"does not have access to this model",
		"model restricted for this project",
		"model entitlement required",
	} {
		t.Run(message, func(t *testing.T) {
			err := &upstreamHTTPError{status: http.StatusForbidden, body: []byte(`{"error":{"message":"` + message + `"}}`)}
			require.True(t, isModelScopedAuthFailure(err))
		})
	}
}

func TestValidateModelCatalogueContinuesAfterModelScopedForbidden(t *testing.T) {
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
		if body.Model == "restricted-model" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"model access denied"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-ok","object":"response"}`))
	}))
	defer endpoint.Close()

	result := validateModelCatalogue(context.Background(), endpoint.Client(), endpoint.URL, "key", []string{"restricted-model", "usable-model-a", "usable-model-b"})

	require.True(t, result.ValidationComplete)
	require.True(t, result.OK)
	require.Equal(t, []string{"usable-model-a", "usable-model-b"}, result.Models)
	require.Equal(t, 3, result.ModelsChecked)
	require.Equal(t, int32(3), requests.Load(), "model-scoped 403 must not stop the remaining catalogue")
}

func TestValidateModelCatalogueAcceptsSuccessfulSSEResponse(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-sse\"}}\n\ndata: [DONE]\n\n"))
	}))
	defer endpoint.Close()

	result := validateModelCatalogue(context.Background(), endpoint.Client(), endpoint.URL, "key", []string{"sse-model"})

	require.True(t, result.ValidationComplete)
	require.True(t, result.OK)
	require.Equal(t, []string{"sse-model"}, result.Models)
	require.Equal(t, []domain.RequestFormat{domain.FormatOpenAIResponses}, result.modelFormats["sse-model"])
	require.Zero(t, result.ModelsFailed)
}

func TestValidateModelCatalogueRetriesChatCompletionTokenParameter(t *testing.T) {
	var chatRequests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/chat/completions":
			request := chatRequests.Add(1)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			w.Header().Set("Content-Type", "application/json")
			if request == 1 {
				require.Contains(t, body, "max_tokens")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"max_tokens is not supported; use max_completion_tokens"}}`))
				return
			}
			require.Contains(t, body, "max_completion_tokens")
			require.NotContains(t, body, "max_tokens")
			_, _ = w.Write([]byte(`{"id":"chat-ok","object":"chat.completion"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	result := validateModelCatalogue(context.Background(), endpoint.Client(), endpoint.URL, "key", []string{"chat-model"})

	require.True(t, result.ValidationComplete)
	require.True(t, result.OK)
	require.Equal(t, []string{"chat-model"}, result.Models)
	require.Zero(t, result.ModelsFailed)
	require.Equal(t, int32(2), chatRequests.Load())
}

func TestValidateModelCatalogueFallsBackWhenModelOnlySupportsChatCompletions(t *testing.T) {
	var chatRequests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"chat-only-model"}]}`))
		case "/v1/responses":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"this model only supports chat completions"}}`))
		case "/v1/chat/completions":
			chatRequests.Add(1)
			_, _ = w.Write([]byte(`{"id":"chat-ok","object":"chat.completion","choices":[{"index":0}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	result := validateModelCatalogue(context.Background(), endpoint.Client(), endpoint.URL, "relay-key", []string{"chat-only-model"})

	require.True(t, result.ValidationComplete)
	require.True(t, result.OK)
	require.Equal(t, []string{"chat-only-model"}, result.Models)
	require.Equal(t, []domain.RequestFormat{domain.FormatOpenAIChat}, result.modelFormats["chat-only-model"])
	require.Equal(t, int32(1), chatRequests.Load())
}
