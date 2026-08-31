// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestSendUpstreamChatProbeRetriesRequiredStreamShape(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		attempt := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			require.Equal(t, false, body["stream"])
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"stream must be true"}}`))
			return
		}
		require.Equal(t, true, body["stream"])
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\"}}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	status, err := sendUpstreamChatProbe(context.Background(), server.Client(), server.URL+"/v1/chat/completions", "relay-key", "chat-model")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, int32(2), requests.Load())
}

func TestTestUpstreamFallsBackWhenResponsesRouteReturnsUnsupported500(t *testing.T) {
	key := "relay-key"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"chat-only-model"}]}`))
		case "/v1/responses":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"unsupported endpoint"}}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"chat-ok","object":"chat.completion","choices":[{}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// The service method also records the result, so use the lightweight store
	// used by the existing upstream probe tests.
	store := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: server.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: store, upstreamHTTPClient: server.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.Equal(t, []string{"/v1/models", "/v1/responses", "/v1/chat/completions"}, paths)
}

func TestShouldFallbackTestRequestRecognizesUnsupportedRoute500(t *testing.T) {
	err := &upstreamHTTPError{status: http.StatusInternalServerError, body: []byte(`{"error":{"message":"unsupported endpoint"}}`)}
	require.True(t, shouldFallbackTestRequest(http.StatusInternalServerError, err))
}
