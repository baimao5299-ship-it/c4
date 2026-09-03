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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Routing reads ModelsCheckedAt as "this model list is the upstream's entire
// capability set" and refuses every model outside it. A probe that timed out
// partway through the catalogue therefore must not stamp it: doing so publishes
// the handful of models probed before the deadline as the whole inventory,
// making the unprobed remainder unroutable and hiding every group whose
// allowlist needs those models.
func TestListUpstreamModelsDoesNotStampCheckedAtOnPartialRun(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"fast-model"},{"id":"slow-model"}]}`))
		case "/v1/responses":
			var request struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			if request.Model == "slow-model" {
				<-r.Context().Done()
				return
			}
			_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key,
	}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	result, err := svc.ListUpstreamModels(ctx, 1)
	require.NoError(t, err)
	require.False(t, result.ValidationComplete, "the slow model must leave the run incomplete")
	require.Equal(t, "timeout", result.ErrorCode)
	require.Contains(t, result.Models, "fast-model", "a confirmed model still joins the routable list")
	require.Nil(t, stub.row.ModelsCheckedAt,
		"an incomplete probe must not claim the snapshot is exhaustive")
	require.NotNil(t, stub.row.ModelsError, "the operator still sees the retry warning")
}

// The counterpart: a run that probed every advertised model is authoritative,
// including when some models failed. Only then may routing narrow to the list.
func TestListUpstreamModelsStampsCheckedAtOnCompleteRun(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"only-model"}]}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key,
	}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}

	result, err := svc.ListUpstreamModels(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.ValidationComplete)
	require.Equal(t, []string{"only-model"}, result.Models)
	require.NotNil(t, stub.row.ModelsCheckedAt, "a complete run is authoritative")
}
