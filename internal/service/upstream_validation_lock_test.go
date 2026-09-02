// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

type validationLockStore struct {
	*multiUpstreamServiceStub
	acquired int32
	released int32
	ok       bool
}

type validationSnapshotStore struct{ *multiUpstreamServiceStub }

func (s *validationSnapshotStore) ListAllUpstreams(context.Context) ([]*domain.Upstream, error) {
	return s.rows, nil
}

func (s *validationLockStore) AcquireUpstreamValidationLock(context.Context) (func(), bool, error) {
	if !s.ok {
		return nil, false, nil
	}
	atomic.AddInt32(&s.acquired, 1)
	return func() { atomic.AddInt32(&s.released, 1) }, true, nil
}

func TestValidationUsesOptionalCrossInstanceLock(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	store := &validationLockStore{
		multiUpstreamServiceStub: &multiUpstreamServiceStub{rows: []*domain.Upstream{{ID: 1, Name: "relay", BaseURL: endpoint.URL}}},
		ok:                       true,
	}
	svc := &Service{upstreams: store, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.PreviewUpstreamModels(context.Background(), endpoint.URL, "Bearer copied-key")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, int32(1), atomic.LoadInt32(&store.acquired))
	require.Equal(t, int32(1), atomic.LoadInt32(&store.released))
}

func TestValidationReportsCrossInstanceLockConflict(t *testing.T) {
	store := &validationLockStore{
		multiUpstreamServiceStub: &multiUpstreamServiceStub{rows: []*domain.Upstream{{ID: 1}}},
	}
	svc := &Service{upstreams: store}
	_, err := svc.PreviewUpstreamModels(context.Background(), "https://relay.example.test", "key")
	require.ErrorIs(t, err, ErrConflict)
	require.Zero(t, atomic.LoadInt32(&store.acquired))
	require.Zero(t, atomic.LoadInt32(&store.released))
}

func TestValidationRejectsOversizedProductionSnapshot(t *testing.T) {
	rows := make([]*domain.Upstream, upstreamValidationSnapshotMax+1)
	for i := range rows {
		rows[i] = &domain.Upstream{ID: int64(i + 1)}
	}
	store := &validationSnapshotStore{multiUpstreamServiceStub: &multiUpstreamServiceStub{rows: rows}}
	_, err := loadUpstreamValidationSnapshot(context.Background(), store)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestValidationRejectsDuplicateProductionSnapshot(t *testing.T) {
	store := &validationSnapshotStore{multiUpstreamServiceStub: &multiUpstreamServiceStub{rows: []*domain.Upstream{{ID: 7}, {ID: 7}}}}
	_, err := loadUpstreamValidationSnapshot(context.Background(), store)
	require.ErrorIs(t, err, repository.ErrConflict)
}

func TestStoredBearerKeyIsNormalizedBeforeEveryProbe(t *testing.T) {
	const key = "copied-key"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"verified","object":"response"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()

	stored := "Bearer " + key
	store := &validationLockStore{
		multiUpstreamServiceStub: &multiUpstreamServiceStub{rows: []*domain.Upstream{{ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &stored}}},
		ok:                       true,
	}
	svc := &Service{upstreams: store, upstreamHTTPClient: endpoint.Client()}
	result, err := svc.ListUpstreamModels(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, result.OK)
}
