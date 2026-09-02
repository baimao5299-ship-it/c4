// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// A 429 proves that the upstream received the probe. Replaying it immediately
// can charge twice, so the validator keeps the snapshot retryable instead.
func TestValidateModelCatalogueDoesNotReplayRateLimitedProbe(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		var body struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"temporary rate limit"}}`))
	}))
	defer server.Close()

	result := validateModelCatalogue(context.Background(), server.Client(), server.URL, "relay-key", []string{"manual-model"})
	require.False(t, result.ValidationComplete)
	require.False(t, result.OK)
	require.Empty(t, result.Models)
	require.Equal(t, "rate_limited", result.ErrorCode)
	require.Equal(t, 1, result.ModelsFailed)
	require.Equal(t, int32(1), requests.Load())
}

func TestShouldRetryTransientModelProbeSkipsDefinitiveErrors(t *testing.T) {
	require.False(t, shouldRetryTransientModelProbe(http.StatusTooManyRequests, &upstreamHTTPError{status: http.StatusTooManyRequests}))
	require.False(t, shouldRetryTransientModelProbe(http.StatusServiceUnavailable, &upstreamHTTPError{status: http.StatusServiceUnavailable}))
	require.False(t, shouldRetryTransientModelProbe(http.StatusUnauthorized, &upstreamHTTPError{status: http.StatusUnauthorized}))
	require.False(t, shouldRetryTransientModelProbe(http.StatusOK, errInvalidUpstreamResponse))
	require.False(t, shouldRetryTransientModelProbe(0, context.DeadlineExceeded))
}

type preSendFailureTransport struct {
	attempts atomic.Int32
}

func (*preSendFailureTransport) SupportsHTTPTrace() bool { return true }

func (t *preSendFailureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.attempts.Add(1) == 1 {
		return nil, errors.New("connection failed before write")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"verified","object":"response"}`)),
		Request:    req,
	}, nil
}

func TestSendUpstreamModelProbeRetriesOnlyProvenPreSendTransportFailure(t *testing.T) {
	transport := &preSendFailureTransport{}
	status, err := sendUpstreamModelProbeWithRetry(
		context.Background(), &http.Client{Transport: transport}, "https://relay.example.test", "relay-key", "model-a",
	)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, int32(2), transport.attempts.Load())
}

func TestSendUpstreamModelProbeRetriesStandardTransportDialFailure(t *testing.T) {
	var dials atomic.Int32
	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("dial failed before write")
		},
	}
	defer transport.CloseIdleConnections()

	status, err := sendUpstreamModelProbeWithRetry(
		context.Background(), &http.Client{Transport: transport}, "http://relay.example.test", "relay-key", "model-a",
	)

	require.Error(t, err)
	require.Zero(t, status)
	require.Equal(t, int32(2), dials.Load())
}

type opaqueFailureTransport struct {
	attempts atomic.Int32
}

func (t *opaqueFailureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.attempts.Add(1)
	return nil, errors.New("opaque transport failure")
}

func TestSendUpstreamModelProbeDoesNotReplayOpaqueTransportFailure(t *testing.T) {
	transport := &opaqueFailureTransport{}
	status, err := sendUpstreamModelProbeWithRetry(
		context.Background(), &http.Client{Transport: transport}, "https://relay.example.test", "relay-key", "model-a",
	)

	require.Error(t, err)
	require.Zero(t, status)
	require.Equal(t, int32(1), transport.attempts.Load())
}

type writtenFailureTransport struct {
	attempts atomic.Int32
	mode     string
}

func (*writtenFailureTransport) SupportsHTTPTrace() bool { return true }

func (t *writtenFailureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.attempts.Add(1)
	if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
		switch t.mode {
		case "headers":
			trace.WroteHeaders()
		case "request":
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		case "write-error":
			trace.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("partial write")})
		}
	}
	return nil, errors.New("connection lost")
}

func TestSendUpstreamModelProbeDoesNotReplayAfterAnyWriteSignal(t *testing.T) {
	for _, mode := range []string{"headers", "request", "write-error"} {
		t.Run(mode, func(t *testing.T) {
			transport := &writtenFailureTransport{mode: mode}
			status, err := sendUpstreamModelProbeWithRetry(
				context.Background(), &http.Client{Transport: transport}, "https://relay.example.test", "relay-key", "model-a",
			)

			require.Error(t, err)
			require.Zero(t, status)
			require.Equal(t, int32(1), transport.attempts.Load())
		})
	}
}

type fallbackThenPreSendFailureTransport struct {
	responses atomic.Int32
	chat      atomic.Int32
}

func (*fallbackThenPreSendFailureTransport) SupportsHTTPTrace() bool { return true }

func (t *fallbackThenPreSendFailureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Path {
	case "/v1/responses":
		t.responses.Add(1)
		if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
			trace.WroteHeaders()
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("route not found")),
			Request:    req,
		}, nil
	case "/v1/chat/completions":
		t.chat.Add(1)
		return nil, errors.New("chat connection failed before write")
	default:
		return nil, errors.New("unexpected route")
	}
}

func TestSendUpstreamModelProbeDoesNotRestartProtocolChainAfterPriorWrite(t *testing.T) {
	transport := &fallbackThenPreSendFailureTransport{}
	status, err := sendUpstreamModelProbeWithRetry(
		context.Background(), &http.Client{Transport: transport}, "https://relay.example.test", "relay-key", "model-a",
	)

	require.Error(t, err)
	require.Zero(t, status)
	require.Equal(t, int32(1), transport.responses.Load())
	require.Equal(t, int32(1), transport.chat.Load())
}
