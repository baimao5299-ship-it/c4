// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

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
)

func TestSendUpstreamMessagesProbeUsesAnthropicWireShape(t *testing.T) {
	var seen atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/messages", r.URL.Path)
		require.Equal(t, "relay-key", r.Header.Get("x-api-key"))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Equal(t, "2023-06-01", r.Header.Get("anthropic-version"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "messages-model", body["model"])
		require.Equal(t, float64(1), body["max_tokens"])
		require.Equal(t, false, body["stream"])
		seen.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-1","type":"message","content":[]}`)
	}))
	defer server.Close()

	status, err := sendUpstreamMessagesProbe(context.Background(), server.Client(), server.URL+"/v1/messages", "relay-key", "messages-model")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, int32(1), seen.Load())
}

func TestSendUpstreamModelProbeFallsBackToMessagesOnlyRelay(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses", "/v1/chat/completions":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/messages":
			require.Equal(t, "x-api-key", strings.ToLower(firstAuthHeader(r.Header)))
			_, _ = io.WriteString(w, `{"id":"msg-1","type":"message","content":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	status, err := sendUpstreamModelProbe(context.Background(), server.Client(), server.URL, "relay-key", "messages-model")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"}, paths)
}

func TestSendUpstreamTestRequestRetriesAlternateAuthHeader(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		if attempt == 1 {
			require.Equal(t, "Bearer relay-key", r.Header.Get("Authorization"))
			require.Empty(t, r.Header.Get("x-api-key"))
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"use x-api-key header"}}`)
			return
		}
		require.Empty(t, r.Header.Get("Authorization"))
		require.Equal(t, "relay-key", r.Header.Get("x-api-key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-1","object":"response"}`)
	}))
	defer server.Close()

	status, err := sendUpstreamTestRequest(context.Background(), server.Client(), server.URL, "relay-key", "model", false)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, int32(2), requests.Load())
}

func TestShouldRetryUpstreamAuthRequiresExplicitHeaderMismatch(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"use x-api-key header"}}`,
		`{"error":{"message":"x-api-key header required"}}`,
		`{"error":{"message":"missing x-api-key"}}`,
		`{"error":{"message":"unsupported authentication scheme; use x-api-key"}}`,
	} {
		err := &upstreamHTTPError{status: http.StatusUnauthorized, body: []byte(body)}
		require.Truef(t, shouldRetryUpstreamAuth(http.StatusUnauthorized, err), "explicit header mismatch %q should retry", body)
	}
	for _, body := range []string{
		`{"error":{"message":"missing Authorization: Bearer"}}`,
		`{"error":{"message":"unsupported authentication scheme; use Bearer"}}`,
	} {
		err := &upstreamHTTPError{status: http.StatusUnauthorized, body: []byte(body)}
		require.Truef(t, shouldRetryUpstreamAuthMode(http.StatusUnauthorized, err, upstreamAuthAPIKey), "explicit Bearer mismatch %q should retry for x-api-key mode", body)
	}
	for _, body := range []string{
		"",
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"invalid token"}}`,
		`{"error":{"message":"unauthorized"}}`,
		`{"error":{"message":"forbidden"}}`,
		`{"error":{"message":"model access denied"}}`,
	} {
		err := &upstreamHTTPError{status: http.StatusUnauthorized, body: []byte(body)}
		require.Falsef(t, shouldRetryUpstreamAuth(http.StatusUnauthorized, err), "generic auth response %q must not retry", body)
	}
}

func TestFetchAdvertisedModelsDoesNotRetryGenericAuthFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer server.Close()

	models, code := fetchAdvertisedModels(context.Background(), server.Client(), server.URL, "relay-key")
	require.Nil(t, models)
	require.Equal(t, "auth", code)
	require.Equal(t, int32(1), requests.Load(), "a definitive auth failure must not try another header or route")
}

func TestFetchAdvertisedModelsRetriesAlternateAuthHeader(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		if attempt == 1 {
			require.Equal(t, "Bearer relay-key", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"use x-api-key header"}}`)
			return
		}
		require.Empty(t, r.Header.Get("Authorization"))
		require.Equal(t, "relay-key", r.Header.Get("x-api-key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"model-a"}]}`)
	}))
	defer server.Close()

	models, code := fetchAdvertisedModels(context.Background(), server.Client(), server.URL, "relay-key")
	require.Empty(t, code)
	require.Equal(t, []string{"model-a"}, models)
	require.Equal(t, int32(2), requests.Load())
}

func TestSendUpstreamMessagesProbeRetriesStreamRequiredShape(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		attempt := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			require.Equal(t, false, body["stream"])
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"stream must be true"}}`)
			return
		}
		require.Equal(t, true, body["stream"])
		_, _ = io.WriteString(w, `{"id":"msg-stream","type":"message","content":[]}`)
	}))
	defer server.Close()

	status, err := sendUpstreamMessagesProbe(context.Background(), server.Client(), server.URL+"/v1/messages", "relay-key", "messages-model")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, int32(2), requests.Load())
}

func TestSendUpstreamMessagesProbeRemovesRejectedOptionalStream(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		attempt := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			require.Equal(t, false, body["stream"])
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"stream is not supported"}}`)
			return
		}
		require.NotContains(t, body, "stream", "optional stream must be removed, not changed to true")
		_, _ = io.WriteString(w, `{"id":"msg-compact","type":"message","content":[]}`)
	}))
	defer server.Close()

	status, err := sendUpstreamMessagesProbe(context.Background(), server.Client(), server.URL+"/v1/messages", "relay-key", "messages-model")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, int32(2), requests.Load())
}

func TestNormalizeUpstreamBaseURLStripsOperationPathWithoutV1(t *testing.T) {
	for _, tc := range []struct {
		base string
		want string
	}{
		{base: "https://relay.example/openai/chat/completions", want: "https://relay.example/openai"},
		{base: "https://relay.example/api/responses", want: "https://relay.example/api"},
		{base: "https://relay.example/messages", want: "https://relay.example"},
		{base: "https://relay.example/openai/v1/messages", want: "https://relay.example/openai"},
	} {
		require.Equal(t, tc.want, normalizeUpstreamBaseURL(tc.base), tc.base)
	}
}

func TestValidateModelCatalogueDoesNotProbeDuplicateIdentifiers(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-1","object":"response"}`)
	}))
	defer server.Close()

	result := validateModelCatalogue(context.Background(), server.Client(), server.URL, "relay-key", []string{"model-a", " model-a ", "model-a"})
	require.True(t, result.ValidationComplete)
	require.True(t, result.OK)
	require.Equal(t, []string{"model-a"}, result.Models)
	require.Equal(t, 1, result.ModelsTotal)
	require.Equal(t, 1, result.ModelsChecked)
	require.Equal(t, int32(1), requests.Load())
}

func TestReadUpstreamProbeBodyAcceptsOpenSSEFrameImmediately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m-1\"}}\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	started := time.Now()
	body, err := readUpstreamProbeBody(response)
	require.NoError(t, err)
	require.Less(t, time.Since(started), 500*time.Millisecond)
	require.True(t, isUpstreamSuccessResponse(body))
}

func TestReadUpstreamProbeBodyAcceptsChunkedJSONWithoutEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.relay+json; charset=utf-8")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		_, _ = io.WriteString(w, `{"id":"resp-keepalive","object":"response"}`)
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	// The handler flushes and keeps the connection alive, so a reader that
	// waits for EOF would take the full context timeout instead of returning.
	started := time.Now()
	body, err := readUpstreamProbeBody(response)
	require.NoError(t, err)
	require.Less(t, time.Since(started), 500*time.Millisecond)
	require.True(t, isUpstreamSuccessResponse(body))
}

func TestIsJSONContentType(t *testing.T) {
	for _, contentType := range []string{
		"application/json",
		"Application/JSON; charset=utf-8",
		"application/vnd.relay+json; charset=utf-8",
		"text/json",
	} {
		require.Truef(t, isJSONContentType(contentType), "expected JSON content type: %q", contentType)
	}
	for _, contentType := range []string{"text/event-stream", "text/plain", "application/xml", ""} {
		require.Falsef(t, isJSONContentType(contentType), "unexpected JSON content type: %q", contentType)
	}
}

func firstAuthHeader(headers http.Header) string {
	if headers.Get("x-api-key") != "" {
		return "x-api-key"
	}
	if headers.Get("Authorization") != "" {
		return "Authorization"
	}
	return ""
}
