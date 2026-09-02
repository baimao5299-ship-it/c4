// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// canceledWriteRejectingStore models a repository that rejects writes using a
// canceled request context. Validation must detach only the bounded persistence
// step, otherwise a client disconnect is reported as a misleading storage error.
type canceledWriteRejectingStore struct {
	*multiUpstreamServiceStub
}

func (s *canceledWriteRejectingStore) RecordUpstreamModels(ctx context.Context, expected *domain.Upstream, models []string, modelErr *string) (*domain.Upstream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.multiUpstreamServiceStub.RecordUpstreamModels(ctx, expected, models, modelErr)
}

func (s *canceledWriteRejectingStore) RecordUpstreamModelCapabilities(ctx context.Context, expected *domain.Upstream, models []string, modelFormats map[string][]domain.RequestFormat, modelErr *string) (*domain.Upstream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.multiUpstreamServiceStub.RecordUpstreamModelCapabilities(ctx, expected, models, modelFormats, modelErr)
}

func (s *canceledWriteRejectingStore) RecordUpstreamProbe(ctx context.Context, expected *domain.Upstream, success bool, latencyMS int64, probeErr *string) (*domain.Upstream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.multiUpstreamServiceStub.RecordUpstreamProbe(ctx, expected, success, latencyMS, probeErr)
}

func TestValidateAllUpstreamsPersistsCanceledDiagnosticWithoutStorageError(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			close(started)
			<-release
			_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	store := &canceledWriteRejectingStore{multiUpstreamServiceStub: &multiUpstreamServiceStub{
		rows: []*domain.Upstream{{ID: 1, Name: "relay", BaseURL: endpoint.URL}},
	}}
	svc := &Service{upstreams: store, upstreamHTTPClient: endpoint.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *UpstreamValidationSummary, 1)
	errDone := make(chan error, 1)
	go func() {
		result, err := svc.ValidateAllUpstreams(ctx)
		done <- result
		errDone <- err
	}()
	<-started
	cancel()
	close(release)

	result := <-done
	require.NoError(t, <-errDone)
	require.Len(t, result.Items, 1)
	item := result.Items[0]
	require.True(t, item.Attempted)
	require.False(t, item.ValidationComplete)
	require.Equal(t, "canceled", item.ErrorCode)
	require.Zero(t, result.Completed)
	require.NotNil(t, store.rows[0].ModelsError)
	require.Equal(t, "canceled", *store.rows[0].ModelsError)
}
