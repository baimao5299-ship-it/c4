// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A provider can legitimately return an empty choices/output array for a
// completion that produced no text (for example a filtered or tool-only probe).
// The completion envelope itself is still evidence that the selected model
// answered; requiring a non-empty content array would hide a manually usable
// model.  Relay wrappers are included because several gateways put the real
// response under data/result before forwarding it to clients.
func TestIsUpstreamSuccessResponseAcceptsEmptyCompletionEnvelopes(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"choices":[]}`),
		[]byte(`{"output":[]}`),
		[]byte(`{"id":"chat-empty","object":"chat.completion","choices":[]}`),
		[]byte(`{"id":"response-empty","object":"response","status":"completed","output":[]}`),
		[]byte(`{"type":"message","id":"message-empty","content":[]}`),
		[]byte(`{"data":{"id":"wrapped-chat","object":"chat.completion","choices":[]}}`),
		[]byte(`{"success":true,"result":{"id":"wrapped-response","object":"response","output":[]}}`),
	} {
		require.Truef(t, isUpstreamSuccessResponse(body), "valid completion envelope %q must be accepted", body)
	}
}

func TestIsUpstreamSuccessResponseAcceptsResponsesAndAnthropicEvents(t *testing.T) {
	for _, body := range [][]byte{
		[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"),
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\ndata: [DONE]\n\n"),
		[]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"ok\"}}\n\n"),
		[]byte("data: {\"data\":{\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}}\n\ndata: [DONE]\n\n"),
	} {
		require.Truef(t, isUpstreamSuccessResponse(body), "provider SSE event %q must be accepted", body)
	}
}

// SSE permits one event payload to span multiple data lines. Some reverse
// proxies wrap long JSON lines this way. The complete event is valid JSON after
// the SSE data lines are joined; parsing each physical line independently would
// turn a manually successful stream into an invalid-response failure.
func TestIsUpstreamSuccessResponseAcceptsMultilineSSEData(t *testing.T) {
	body := []byte("event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\n" +
		"data: \"response\":{\"id\":\"resp-multiline\"}}\n\n")
	require.True(t, isUpstreamSuccessResponse(body), "a valid multiline SSE event must be accepted")
}

// A stream-capable relay may ignore stream=false and keep the HTTP connection
// open after sending its first event. The capability probe only needs the first
// valid provider frame; waiting for EOF makes the same model fail with a
// deadline while a normal client succeeds immediately.
func TestSendUpstreamTestRequestAcceptsFirstSSEFrameBeforeStreamCloses(t *testing.T) {
	frameSent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, "event: response.output_text.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		flusher.Flush()
		close(frameSent)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	status, err := sendUpstreamTestRequest(ctx, server.Client(), server.URL, "relay-key", "manual-model", false)

	select {
	case <-frameSent:
	case <-time.After(time.Second):
		t.Fatal("test relay did not send the first SSE frame")
	}
	require.NoError(t, err, "the first valid SSE frame proves capability without waiting for EOF")
	require.Equal(t, http.StatusOK, status)
}

// A few HTTP proxies strip or rewrite Content-Type while forwarding a stream.
// The wire body still has an SSE data frame, so capability detection should not
// depend solely on that header before deciding to stop reading.
func TestSendUpstreamTestRequestDetectsSSEWithRewrittenContentType(t *testing.T) {
	frameSent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-proxy\"}}\n\n")
		flusher.Flush()
		close(frameSent)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	status, err := sendUpstreamTestRequest(ctx, server.Client(), server.URL, "relay-key", "manual-model", false)

	select {
	case <-frameSent:
	case <-time.After(time.Second):
		t.Fatal("test relay did not send the first SSE frame")
	}
	require.NoError(t, err, "SSE detection should survive a proxy-rewritten Content-Type")
	require.Equal(t, http.StatusOK, status)
}

// A non-SSE relay can flush a complete JSON envelope and leave its chunked
// connection open for keep-alive. The probe must accept the complete line
// immediately; waiting for EOF would turn the same manually usable model into
// a timeout.
func TestSendUpstreamTestRequestAcceptsCompleteJSONBeforeConnectionClose(t *testing.T) {
	frameSent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, `{"id":"json-keepalive","object":"response"}`+"\n")
		flusher.Flush()
		close(frameSent)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	status, err := sendUpstreamTestRequest(ctx, server.Client(), server.URL, "relay-key", "manual-model", false)

	select {
	case <-frameSent:
	case <-time.After(time.Second):
		t.Fatal("test relay did not send the JSON response")
	}
	require.NoError(t, err, "a complete JSON response must not wait for connection close")
	require.Equal(t, http.StatusOK, status)
}

// A 503 response means the request reached the relay and may already have
// consumed upstream work. Keep the result transient without replaying it.
func TestValidateModelCatalogueDoesNotReplayTransient503ModelProbe(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"temporary provider outage"}}`)
	}))
	defer server.Close()

	result := validateModelCatalogue(context.Background(), server.Client(), server.URL, "relay-key", []string{"manual-model"})
	require.False(t, result.ValidationComplete)
	require.False(t, result.OK)
	require.Empty(t, result.Models)
	require.Equal(t, "upstream", result.ErrorCode)
	require.Equal(t, 1, result.ModelsFailed)
	require.Equal(t, int32(1), requests.Load())
}

func TestParseUpstreamModelsPayloadAcceptsCommonRelayWrappers(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{name: "openai list", body: `{"object":"list","data":[{"id":"gpt-a"}]}`, want: []string{"gpt-a"}},
		{name: "models array", body: `{"models":[{"name":"model-b"}]}`, want: []string{"model-b"}},
		{name: "result data", body: `{"success":true,"result":{"data":[{"model":"model-c"}]}}`, want: []string{"model-c"}},
		{name: "data models", body: `{"data":{"models":[{"id":"model-d"}]}}`, want: []string{"model-d"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			models, recognized := parseUpstreamModelsPayload([]byte(tc.body))
			require.True(t, recognized)
			require.Equal(t, tc.want, models)
		})
	}
}
