// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestManualModelSuccessClearsPriorCatalogueError(t *testing.T) {
	key := "relay-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"manual-ok","object":"response"}`))
	}))
	defer server.Close()

	priorError := "timeout"
	store := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: server.URL, UpstreamKey: &key,
		Models: []string{"model-old"}, ModelsError: &priorError,
	}}
	svc := &Service{upstreams: store, upstreamHTTPClient: server.Client()}

	result, err := svc.TestUpstreamWithModel(context.Background(), 1, "manual-model")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.NotNil(t, result.Upstream)
	require.Nil(t, result.Upstream.ModelsError, "a successful explicit model probe clears stale upstream-level errors")
	require.Equal(t, []string{"model-old", "manual-model"}, result.Upstream.Models)
}

func TestUpstreamModelValidationNormalizesCopiedOperationEndpoint(t *testing.T) {
	key := "relay-key"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"response-ok","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	store := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: server.URL + "/v1/chat/completions", UpstreamKey: &key,
	}}
	svc := &Service{upstreams: store, upstreamHTTPClient: server.Client()}

	result, err := svc.ListUpstreamModels(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, []string{"model-a"}, result.Models)
	require.Equal(t, []string{"/v1/models", "/v1/responses"}, paths)
}
