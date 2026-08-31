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

// A transient timeout after one successful probe must not delete a model that
// was verified by an earlier manual check. The current success is added to the
// retained snapshot and the timeout remains visible as a warning.
func TestListUpstreamModelsRetainsPreviousSnapshotOnPartialRun(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"new-model"},{"id":"slow-model"}]}`))
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

	oldModels := []string{"manual-model"}
	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key, Models: oldModels,
	}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	result, err := svc.ListUpstreamModels(ctx, 1)
	require.NoError(t, err)
	require.False(t, result.ValidationComplete)
	require.Equal(t, "timeout", result.ErrorCode)
	require.Equal(t, []string{"manual-model", "new-model"}, result.Models)
	require.Equal(t, 2, result.ModelsAvailable)
	require.Equal(t, result.Models, stub.row.Models)
	require.NotNil(t, stub.row.ModelsError)
}

// Batch validation uses the same retention rule and must report an upstream
// with a known-good snapshot as usable even when this run is incomplete.
func TestValidateAllUpstreamsMarksRetainedSnapshotUsable(t *testing.T) {
	key := "relay-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"slow-model"}]}`))
		case "/v1/responses":
			// Keep the fixture finite. The client deadline should cancel the
			// request first, but the handler must also let httptest.Server.Close
			// finish even when the transport keeps the socket briefly alive.
			time.Sleep(150 * time.Millisecond)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	stub := &multiUpstreamServiceStub{rows: []*domain.Upstream{{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &key, Models: []string{"manual-model"}, Enabled: true,
	}}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	summary, err := svc.ValidateAllUpstreams(ctx)
	require.NoError(t, err)
	require.Len(t, summary.Items, 1)
	item := summary.Items[0]
	require.True(t, item.Attempted)
	require.False(t, item.ValidationComplete)
	require.True(t, item.OK)
	require.Equal(t, []string{"manual-model"}, item.Models)
	require.Equal(t, "timeout", item.ErrorCode)
	require.Equal(t, []string{"manual-model"}, stub.rows[0].Models)
}
