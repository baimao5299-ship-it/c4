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

func TestFetchAdvertisedModelsFollowsSameHostTrailingSlashRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			http.Redirect(w, r, "/v1/models/", http.StatusMovedPermanently)
		case "/v1/models/":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"redirect-model"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	models, code := fetchAdvertisedModels(context.Background(), &http.Client{CheckRedirect: upstreamCheckRedirect}, server.URL, "relay-key")
	require.Empty(t, code)
	require.Equal(t, []string{"redirect-model"}, models)
}

func TestFetchAdvertisedModelsRetriesTransientCatalogueFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"recovered-model"}]}`))
	}))
	defer server.Close()

	models, code := fetchAdvertisedModels(context.Background(), server.Client(), server.URL, "relay-key")
	require.Empty(t, code)
	require.Equal(t, []string{"recovered-model"}, models)
	require.Equal(t, int32(2), requests.Load())
}

func TestUpstreamRedirectRejectsCrossHostBeforeSendingCredential(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		require.Empty(t, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/v1/models", http.StatusFound)
	}))
	defer origin.Close()

	client := &http.Client{CheckRedirect: upstreamCheckRedirect}
	models, code := fetchAdvertisedModels(context.Background(), client, origin.URL, "relay-key")
	require.Nil(t, models)
	require.Equal(t, "http_error", code)
	require.Zero(t, redirected.Load(), "cross-host redirect must not be followed")
}

func TestSendUpstreamResponsesProbeRetriesArrayInputShape(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		request := requests.Add(1)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		if request == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"input must be an array"}}`))
			return
		}
		_, ok := body["input"].([]any)
		require.True(t, ok, "compatibility retry must use an input array")
		_, _ = w.Write([]byte(`{"id":"resp-array","object":"response"}`))
	}))
	defer server.Close()

	status, err := sendUpstreamResponsesProbe(context.Background(), server.Client(), server.URL+"/v1/responses", "relay-key", "array-model")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, int32(2), requests.Load())
}

func TestSendUpstreamResponsesProbeRetriesStreamShape(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := requests.Add(1)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if request == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"stream must be true"}}`))
			return
		}
		require.Equal(t, true, body["stream"])
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stream\"}}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	status, err := sendUpstreamResponsesProbe(context.Background(), server.Client(), server.URL+"/v1/responses", "relay-key", "stream-model")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, int32(2), requests.Load())
}

func TestSendUpstreamResponsesProbeDropsRejectedOutputLimit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := requests.Add(1)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		if request == 1 {
			require.Contains(t, body, "max_output_tokens")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown parameter max_output_tokens"}}`))
			return
		}
		require.NotContains(t, body, "max_output_tokens")
		_, _ = w.Write([]byte(`{"id":"resp-default-limit","object":"response"}`))
	}))
	defer server.Close()

	status, err := sendUpstreamResponsesProbe(context.Background(), server.Client(), server.URL+"/v1/responses", "relay-key", "default-limit-model")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, int32(2), requests.Load())
}
