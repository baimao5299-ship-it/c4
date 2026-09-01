// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestParseUpstreamModelsPayloadAcceptsRelayModelIdentifierFields(t *testing.T) {
	body := []byte(`{"data":[{"model_id":"model-id"},{"model_name":"model-name"},{"slug":"model-slug"},{"name":"Display title","slug":"canonical-slug"}]}`)
	models, recognized := parseUpstreamModelsPayload(body)
	require.True(t, recognized)
	require.Equal(t, []string{"model-id", "model-name", "model-slug", "canonical-slug"}, models)
}

func TestParseUpstreamModelsPayloadFallsBackWhenPreferredIdentifierIsEmpty(t *testing.T) {
	body := []byte(`{"data":[
		{"model_id":"", "id":"usable-id"},
		{"slug":"   ", "model":"usable-model"},
		{"model_id":"  ", "name":"usable-name"}
	]}`)
	models, recognized := parseUpstreamModelsPayload(body)
	require.True(t, recognized)
	require.Equal(t, []string{"usable-id", "usable-model", "usable-name"}, models)
}

func TestParseUpstreamModelsPayloadRejectsMalformedModelEntries(t *testing.T) {
	for _, body := range []string{
		`{"data":[{"id":123}]}`,
		`{"data":[{"model_id":true}]}`,
		`{"data":[{"slug":""}]}`,
		`{"data":[null]}`,
	} {
		models, recognized := parseUpstreamModelsPayload([]byte(body))
		require.Falsef(t, recognized, "malformed catalogue %s must be rejected", body)
		require.Nil(t, models)
	}
}

// A relay may omit aliases from /models while accepting them on the actual
// completion route. Explicit manual testing should use the provider response
// as the authority instead of rejecting the request before it is sent.
func TestTestUpstreamWithModelProbesExplicitModelMissingFromCatalogue(t *testing.T) {
	key := "relay-key"
	var probeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"listed-model"}]}`))
		case "/v1/responses":
			probeCalls++
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "hidden-alias", body["model"])
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"manual-response","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	store := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: server.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: store, upstreamHTTPClient: server.Client()}

	result, err := svc.TestUpstreamWithModel(context.Background(), 1, "hidden-alias")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.Equal(t, 1, probeCalls)
	require.Equal(t, []string{"hidden-alias"}, store.row.Models, "a successful explicit probe must be routable after reload")
	require.NotNil(t, store.row.ModelsCheckedAt)
}

func TestTestUpstreamWithModelDoesNotDependOnCatalogueAvailabilityWhenExplicit(t *testing.T) {
	key := "relay-key"
	var probeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/v1/responses":
			probeCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"manual-response","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	store := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: server.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: store, upstreamHTTPClient: server.Client()}

	result, err := svc.TestUpstreamWithModel(context.Background(), 1, "known-manual-model")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.Equal(t, 1, probeCalls)
	require.Equal(t, []string{"known-manual-model"}, store.row.Models, "manual success must survive a temporary catalogue outage")
}
