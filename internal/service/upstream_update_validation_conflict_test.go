// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestUpdateUpstreamWithModelValidationRejectsStaleRevisionBeforeProbe(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer endpoint.Close()

	oldKey := "old-key"
	currentRevision := time.Now().UTC().Add(-time.Second)
	staleRevision := currentRevision.Add(-time.Minute)
	stub := &upstreamServiceStub{row: &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &oldKey,
		MultiplierBP: 10000, UpdatedAt: currentRevision,
	}}
	svc := &Service{upstreams: stub, upstreamHTTPClient: endpoint.Client()}
	newKey := "new-key"
	_, err := svc.UpdateUpstreamWithModelValidation(context.Background(), &domain.Upstream{
		ID: 1, Name: "relay", BaseURL: endpoint.URL, UpstreamKey: &newKey,
		MultiplierBP: 10000, ExpectedUpdatedAt: &staleRevision,
	})
	require.True(t, errors.Is(err, ErrConflict), "stale updates must be rejected before a paid probe: %v", err)
	require.Zero(t, calls.Load(), "a stale revision must not trigger model discovery or validation")
}
