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

func TestParseUpstreamModelsPayloadAcceptsCamelCaseAndKeyedCatalogues(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{name: "camel case", body: `{"data":[{"modelId":"gpt-camel"},{"modelName":"claude-camel"},{"ID":"upper-id"}]}`, want: []string{"gpt-camel", "claude-camel", "upper-id"}},
		{name: "keyed models", body: `{"models":{"gpt-keyed":{"owned_by":"openai"},"claude-keyed":{"owned_by":"anthropic"}}}`, want: []string{"gpt-keyed", "claude-keyed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models, recognized := parseUpstreamModelsPayload([]byte(tc.body))
			require.True(t, recognized)
			require.ElementsMatch(t, tc.want, models)
		})
	}
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
	// Probing one explicit model is not a catalogue validation, so it must not
	// stamp ModelsCheckedAt. Routing reads that timestamp as "this list is the
	// upstream's entire capability set": stamping here would leave only
	// hidden-alias routable and turn the advertised listed-model unroutable,
	// which is the opposite of what the assertion above asks for. Leaving it nil
	// keeps the endpoint permissive, so both the alias and the catalogue route.
	// The recorded model still matters -- a later full validation merges the
	// retained snapshot, so this manually confirmed alias survives.
	require.Nil(t, store.row.ModelsCheckedAt,
		"an explicit single-model probe must not claim the catalogue is exhaustive")
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
