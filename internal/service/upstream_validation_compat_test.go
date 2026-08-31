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

func TestPreviewUpstreamModelsRetriesResponsesOptionalFieldRejection(t *testing.T) {
	var responses atomic.Int32
	var chats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"relay-model"}]}`))
		case "/v1/responses":
			responses.Add(1)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if _, hasStore := body["store"]; hasStore {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"unknown field store"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"resp-1","object":"response"}`))
		case "/v1/chat/completions":
			chats.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	result, err := (&Service{upstreamHTTPClient: server.Client()}).PreviewUpstreamModels(context.Background(), server.URL, "relay-key")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, []string{"relay-model"}, result.Models)
	require.Equal(t, int32(2), responses.Load(), "the compact retry should be the only second Responses request")
	require.Zero(t, chats.Load(), "a field rejection is fixed on Responses without a paid Chat fallback")
}

func TestPreviewUpstreamModelsRetriesResponsesOptionalFieldEnvelope(t *testing.T) {
	var responses atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"relay-model"}]}`))
		case "/v1/responses":
			responses.Add(1)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if _, hasStore := body["store"]; hasStore {
				// Some relays use HTTP 200 for application-level validation errors.
				_, _ = w.Write([]byte(`{"error":{"message":"unsupported field store"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"resp-1","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	result, err := (&Service{upstreamHTTPClient: server.Client()}).PreviewUpstreamModels(context.Background(), server.URL, "relay-key")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, []string{"relay-model"}, result.Models)
	require.Equal(t, int32(2), responses.Load())
}

func TestPreviewUpstreamModelsRetriesChatTokenParameterRejection(t *testing.T) {
	var chatRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"chat-model"}]}`))
		case "/v1/responses":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/chat/completions":
			chatRequests.Add(1)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if _, hasLegacy := body["max_tokens"]; hasLegacy {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"unknown parameter max_tokens"}}`))
				return
			}
			require.Equal(t, float64(1), body["max_completion_tokens"])
			_, _ = w.Write([]byte(`{"id":"chat-1","object":"chat.completion","choices":[{"index":0}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	result, err := (&Service{upstreamHTTPClient: server.Client()}).PreviewUpstreamModels(context.Background(), server.URL, "relay-key")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, []string{"chat-model"}, result.Models)
	require.Equal(t, int32(2), chatRequests.Load())
}

func TestPreviewUpstreamModelsAcceptsSSEDespiteStreamFalse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"sse-model"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-sse\"}}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	result, err := (&Service{upstreamHTTPClient: server.Client()}).PreviewUpstreamModels(context.Background(), server.URL, "relay-key")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, []string{"sse-model"}, result.Models)
	require.Empty(t, result.ErrorCode)
}
