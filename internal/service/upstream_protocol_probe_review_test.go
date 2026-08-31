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

// Strict relays can reject more than one request field in sequence. The
// compatibility probe must carry each accepted correction into the next retry;
// otherwise the later shape silently reintroduces the earlier invalid field.
func TestSendUpstreamResponsesProbeAccumulatesCompatibilityShapes(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		attempt := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch attempt {
		case 1:
			require.Equal(t, "hi", body["input"])
			require.Equal(t, false, body["stream"])
			require.Equal(t, float64(1), body["max_output_tokens"])
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"input must be an array"}}`))
		case 2:
			_, ok := body["input"].([]any)
			require.True(t, ok)
			require.Equal(t, false, body["stream"])
			require.Equal(t, float64(1), body["max_output_tokens"])
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"stream must be true"}}`))
		case 3:
			_, ok := body["input"].([]any)
			require.True(t, ok)
			require.Equal(t, true, body["stream"])
			require.Equal(t, float64(1), body["max_output_tokens"])
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown parameter max_output_tokens"}}`))
		case 4:
			_, ok := body["input"].([]any)
			require.True(t, ok)
			require.Equal(t, true, body["stream"])
			require.NotContains(t, body, "max_output_tokens")
			_, _ = w.Write([]byte(`{"type":"response.completed","response":{"id":"resp-ok"}}`))
		default:
			t.Fatalf("unexpected probe attempt %d", attempt)
		}
	}))
	defer server.Close()

	status, err := sendUpstreamResponsesProbe(context.Background(), server.Client(), server.URL+"/v1/responses", "relay-key", "strict-model")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, int32(4), requests.Load())
}

func TestIsJSONObjectResponseAcceptsAnthropicStreamEvents(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"type":"message_start","message":{"id":"msg-1"}}`),
		[]byte(`{"type":"content_block_delta","delta":{"text":"hi"}}`),
	} {
		require.Truef(t, isJSONObjectResponse(body), "Anthropic event %q must be accepted", body)
	}
	for _, body := range [][]byte{
		[]byte(`{"type":"message_error","error":{"message":"provider unavailable"}}`),
	} {
		require.Falsef(t, isJSONObjectResponse(body), "error event %q must be rejected", body)
	}
}
