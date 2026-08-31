// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Some relays send standard SSE metadata before the first data frame. If a
// proxy rewrites Content-Type to application/json, treating an `id:` or
// `retry:` preamble as ordinary JSON makes the validator wait for stream EOF
// and report a timeout even though a manual streaming request succeeds.
func TestSendUpstreamTestRequestRecognizesSSEMetadataPreamble(t *testing.T) {
	frameSent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		_, _ = io.WriteString(w, "id: relay-event\nretry: 1000\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-preamble\"}}\n\n")
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
	require.NoError(t, err, "SSE metadata must not turn a valid frame into a timeout")
	require.Equal(t, http.StatusOK, status)
}

// A UTF-8 BOM and an initial blank separator are both tolerated by SSE
// consumers in the wild. The probe should sniff past those harmless prefixes
// when Content-Type has been rewritten by an intermediary.
func TestSendUpstreamTestRequestRecognizesSSEAfterBOMAndBlankPrefix(t *testing.T) {
	frameSent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		_, _ = io.WriteString(w, "\n\n\ufeffdata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-bom\"}}\n\n")
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
	require.NoError(t, err, "SSE BOM/blank prefix must not cause a false timeout")
	require.Equal(t, http.StatusOK, status)
}
