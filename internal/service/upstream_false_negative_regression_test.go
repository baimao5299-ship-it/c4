// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// Some Chat-only relays report the unsupported Responses route as 403 instead
// of 404/405. The message is the differentiator: an ordinary credential or
// model permission failure must remain an auth result and must not be replayed.
func TestShouldFallbackTestRequestAllowsExplicitChatOnlyForbidden(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"This model only supports Chat Completions"}}`,
		`{"error":{"message":"Responses API is not supported; use Chat Completions"}}`,
		"403: chat completion only",
	} {
		err := &upstreamHTTPError{status: http.StatusForbidden, body: []byte(body)}
		require.Truef(t, shouldFallbackTestRequest(http.StatusForbidden, err), "body %q must select Chat Completions", body)
	}
	for _, body := range []string{
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"model access denied"}}`,
		`{"error":{"message":"permission denied"}}`,
	} {
		err := &upstreamHTTPError{status: http.StatusForbidden, body: []byte(body)}
		require.Falsef(t, shouldFallbackTestRequest(http.StatusForbidden, err), "body %q must remain an auth failure", body)
	}
}

// This is the user-visible regression: the same model is usable through Chat
// Completions, but a Responses-first management probe used to stop at 403 and
// report auth without trying the compatible route.
func TestTestUpstreamFallsBackWhenResponsesForbiddenChatOnly(t *testing.T) {
	key := "relay-key"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"chat-only-model"}]}`)
		case "/v1/responses":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"model only supports Chat Completions"}}`)
		case "/v1/chat/completions":
			_, _ = io.WriteString(w, `{"id":"chat-ok","object":"chat.completion","choices":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	store := &upstreamServiceStub{row: &domain.Upstream{ID: 1, Name: "relay", BaseURL: server.URL, UpstreamKey: &key}}
	svc := &Service{upstreams: store, upstreamHTTPClient: server.Client()}
	result, err := svc.TestUpstream(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Empty(t, result.ErrorCode)
	require.Equal(t, []string{"/v1/models", "/v1/responses", "/v1/chat/completions"}, paths)
}

// A relay may expose /v1/responses only as a WebSocket endpoint. The HTTP
// catalogue is still valid and the same model can work through Chat; the
// validator must detect that transport once instead of timing out every model
// on an unusable Responses POST.
func TestValidateUpstreamModelsSkipsWebSocketOnlyResponsesRoute(t *testing.T) {
	key := "relay-key"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			_, _ = io.WriteString(w, `{"id":"chat-ok","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	status, format, err := sendUpstreamModelProbeWithPreferredShape(withResponsesProbeRouteDisabled(context.Background()), server.Client(), server.URL, key, "gpt-6-astra", responsesProbeCanonical)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, domain.FormatOpenAIChat, format)
	require.Equal(t, []string{
		http.MethodPost + " /v1/chat/completions",
	}, paths)
}

type responsesTimeoutRouteTransport struct {
	paths *[]string
}

func (t responsesTimeoutRouteTransport) SupportsHTTPTrace() bool { return true }

func (t responsesTimeoutRouteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*t.paths = append(*t.paths, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
		return nil, context.DeadlineExceeded
	case r.Method == http.MethodGet && r.URL.Path == "/v1/responses":
		return &http.Response{
			StatusCode: http.StatusUpgradeRequired,
			Header:     http.Header{"Upgrade": []string{"websocket"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"WebSocket upgrade required"}}`)),
			Request:    r,
		}, nil
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"chat-ok","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)),
			Request:    r,
		}, nil
	default:
		return nil, errors.New("unexpected probe route")
	}
}

func TestValidateModelCatalogueRecoversAfterResponsesTimeoutOnWebSocketRoute(t *testing.T) {
	paths := []string{}
	client := &http.Client{Transport: responsesTimeoutRouteTransport{paths: &paths}}
	result := validateModelCatalogue(context.Background(), client, "https://relay.example.test", "relay-key", []string{"gpt-6-astra"})
	require.True(t, result.ValidationComplete)
	require.True(t, result.OK)
	require.Equal(t, []string{"gpt-6-astra"}, result.Models)
	require.Equal(t, []string{
		http.MethodPost + " /v1/responses",
		http.MethodGet + " /v1/responses",
		http.MethodPost + " /v1/chat/completions",
	}, paths)
}

func TestParseUpstreamModelsPayloadAcceptsUTF8BOM(t *testing.T) {
	body := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"data":[{"id":"bom-model"}]}`)...)
	models, recognized := parseUpstreamModelsPayload(body)
	require.True(t, recognized)
	require.Equal(t, []string{"bom-model"}, models)
}

func TestIsJSONObjectResponseAcceptsUTF8BOM(t *testing.T) {
	body := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"id":"bom-response","object":"response"}`)...)
	require.True(t, isJSONObjectResponse(body))
}

func TestParseUpstreamModelsPayloadPrefersCallableIdentifier(t *testing.T) {
	body := []byte(`{"data":[{"id":"internal-uuid","model_id":"callable-model","slug":"display-slug","name":"Display name"}]}`)
	models, recognized := parseUpstreamModelsPayload(body)
	require.True(t, recognized)
	require.Equal(t, []string{"callable-model"}, models)
}

func TestParseUpstreamModelsPayloadSupportsBoundedNestedWrappers(t *testing.T) {
	body := []byte(`{"payload":{"result":{"data":{"models":[{"id":"nested-model"}]}}}}`)
	models, recognized := parseUpstreamModelsPayload(body)
	require.True(t, recognized)
	require.Equal(t, []string{"nested-model"}, models)
}

func TestParseUpstreamModelsPayloadRejectsMalformedNestedEntry(t *testing.T) {
	body := []byte(`{"payload":{"result":{"data":[{"id":"good-model"},{"id":123}]}}}`)
	models, recognized := parseUpstreamModelsPayload(body)
	require.False(t, recognized)
	require.Nil(t, models)
}
