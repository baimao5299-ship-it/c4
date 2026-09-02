// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestPGUpstreamProbeAtomicCounters exercises the production PostgreSQL path
// with concurrent probes. RecordUpstreamProbe must update every cumulative
// counter in one UPDATE so no increment or maximum is lost under contention.
func TestPGUpstreamProbeAtomicCounters(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u, err := repos.Upstreams.CreateUpstream(ctx, &domain.Upstream{
		Name:         "atomic-probe",
		BaseURL:      "https://relay.example.com",
		MultiplierBP: 10000,
		Enabled:      true,
	})
	require.NoError(t, err)

	const workers = 32
	latencies := make([]int64, workers)
	for i := range latencies {
		latencies[i] = int64(25 + i*17)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i, latency := range latencies {
		wg.Add(1)
		go func(i int, latency int64) {
			defer wg.Done()
			success := i%4 != 0
			var probeErr *string
			if !success {
				msg := "network"
				probeErr = &msg
			}
			_, callErr := repos.Upstreams.RecordUpstreamProbe(ctx, u, success, latency, probeErr)
			if callErr != nil {
				errCh <- callErr
			}
		}(i, latency)
	}
	wg.Wait()
	close(errCh)
	for callErr := range errCh {
		require.NoError(t, callErr)
	}

	got, err := repos.Upstreams.GetUpstream(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(workers), got.RequestCount)
	require.Equal(t, int64(workers-workers/4), got.SuccessCount)
	require.Equal(t, int64(workers/4), got.FailureCount)

	var total int64
	var max int64
	for _, latency := range latencies {
		total += latency
		if latency > max {
			max = latency
		}
	}
	require.Equal(t, total, got.LatencyTotalMS)
	require.Equal(t, max, got.LatencyMaxMS)
	require.NotNil(t, got.LastCheckedAt)
	require.NotNil(t, got.LastSuccessAt)
	require.NotNil(t, got.LastFailureAt)
	// The final writer is intentionally nondeterministic: a successful probe
	// clears LastError, while a failed probe records the bounded class.
	if got.LastError != nil {
		require.Equal(t, "network", *got.LastError)
	}

	oldConfig := *u
	newKey := "new-relay-key"
	next := *u
	next.BaseURL = "https://relay-new.example.com"
	next.UpstreamKey = &newKey
	next.ResetTelemetry = true
	_, err = repos.Upstreams.UpdateUpstream(ctx, &next)
	require.NoError(t, err)
	_, err = repos.Upstreams.RecordUpstreamProbe(ctx, &oldConfig, true, 1, nil)
	require.ErrorIs(t, err, repository.ErrConflict)
	got, err = repos.Upstreams.GetUpstream(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, "https://relay-new.example.com", got.BaseURL)
	require.Zero(t, got.RequestCount)

	revision, err := repos.Upstreams.CreateUpstream(ctx, &domain.Upstream{
		Name:         "revision-probe",
		BaseURL:      "https://revision.example.com",
		MultiplierBP: 10000,
		Enabled:      true,
	})
	require.NoError(t, err)
	version := revision.UpdatedAt
	changed := *revision
	changed.MultiplierBP = 8000
	changed.ExpectedUpdatedAt = &version
	changedResult, err := repos.Upstreams.UpdateUpstream(ctx, &changed)
	require.NoError(t, err)
	require.Equal(t, 8000, changedResult.MultiplierBP)

	stale := *revision
	stale.MultiplierBP = 7000
	stale.ExpectedUpdatedAt = &version
	_, err = repos.Upstreams.UpdateUpstream(ctx, &stale)
	require.ErrorIs(t, err, repository.ErrConflict)

	statusOnly, err := repos.Upstreams.SetUpstreamEnabled(ctx, revision.ID, false)
	require.NoError(t, err)
	require.False(t, statusOnly.Enabled)
	require.Equal(t, 8000, statusOnly.MultiplierBP)

	// A deleted record must not accept a late probe update.
	require.NoError(t, repos.Upstreams.DeleteUpstream(ctx, u.ID))
	_, err = repos.Upstreams.RecordUpstreamProbe(ctx, &next, true, 1, nil)
	require.Error(t, err)
	require.Contains(t, fmt.Sprint(err), "id=")
}

// A failed first model read must remain an unknown catalogue. Persisting an
// empty list with a timestamp makes the scheduler interpret the transient
// failure as a confirmed "no models" result and removes every route in an
// upstream group until a later manual probe succeeds.
func TestPGUpstreamModelFailureKeepsUnknownCatalogue(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u, err := repos.Upstreams.CreateUpstream(ctx, &domain.Upstream{
		Name: "model-failure", BaseURL: "https://relay.example.com",
		MultiplierBP: 10000, Enabled: true,
	})
	require.NoError(t, err)
	errorCode := "timeout"
	_, err = repos.Upstreams.RecordUpstreamModels(ctx, u, nil, &errorCode)
	require.NoError(t, err)
	got, err := repos.Upstreams.GetUpstream(ctx, u.ID)
	require.NoError(t, err)
	require.Nil(t, got.ModelsCheckedAt)
	require.Empty(t, got.Models)
	require.NotNil(t, got.ModelsError)
	require.Equal(t, "timeout", *got.ModelsError)
}

func TestPGUpstreamModelCapabilitiesRoundTripAndReset(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u, err := repos.Upstreams.CreateUpstream(ctx, &domain.Upstream{
		Name: "model-capabilities", BaseURL: "https://relay.example.com",
		MultiplierBP: 10000, Enabled: true,
	})
	require.NoError(t, err)
	require.NotNil(t, u.ModelFormats)
	require.Empty(t, u.ModelFormats, "new rows use the backward-compatible unknown object")

	formats := map[string][]domain.RequestFormat{
		"chat-only": {domain.FormatOpenAIChat},
		"multi":     {domain.FormatOpenAIResponses, domain.FormatAnthropic},
	}
	saved, err := repos.RecordUpstreamModelCapabilities(ctx, u, []string{"chat-only", "multi"}, formats, nil)
	require.NoError(t, err)
	require.Equal(t, formats, saved.ModelFormats)
	require.NotNil(t, saved.ModelsCheckedAt)

	// Mutating the caller's map after the write must not mutate the returned or
	// persisted capability snapshot.
	formats["chat-only"][0] = domain.FormatOpenAIResponses
	formats["new"] = []domain.RequestFormat{domain.FormatAnthropic}
	require.Equal(t, []domain.RequestFormat{domain.FormatOpenAIChat}, saved.ModelFormats["chat-only"])
	require.NotContains(t, saved.ModelFormats, "new")
	got, err := repos.GetUpstream(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, []domain.RequestFormat{domain.FormatOpenAIChat}, got.ModelFormats["chat-only"])
	require.NotContains(t, got.ModelFormats, "new")

	// An incomplete validation records its error but keeps both halves of the
	// previous capability snapshot.
	errorCode := "timeout"
	failed, err := repos.RecordUpstreamModelCapabilities(ctx, got, nil, nil, &errorCode)
	require.NoError(t, err)
	require.Equal(t, got.Models, failed.Models)
	require.Equal(t, got.ModelFormats, failed.ModelFormats)

	// Moving to another endpoint invalidates all telemetry, including protocol
	// capabilities from the previous provider.
	next := *failed
	next.BaseURL = "https://relay-new.example.com"
	next.ResetTelemetry = true
	next.Models = nil
	next.ModelFormats = nil
	next.ModelsCheckedAt = nil
	reset, err := repos.UpdateUpstream(ctx, &next)
	require.NoError(t, err)
	require.Nil(t, reset.ModelsCheckedAt)
	require.Empty(t, reset.Models)
	require.NotNil(t, reset.ModelFormats)
	require.Empty(t, reset.ModelFormats)
}

// TestPGUpstreamValidationAdvisoryLock is the multi-instance guard for the
// long-running model probe. The lock must survive pool connection reuse until
// the explicit release and become available again afterward.
func TestPGUpstreamValidationAdvisoryLock(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	releaseA, ok, err := repos.AcquireUpstreamValidationLock(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, releaseA)

	releaseB, ok, err := repos.AcquireUpstreamValidationLock(ctx)
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, releaseB)

	releaseA()
	releaseA() // idempotent release must not return a pooled connection twice

	releaseC, ok, err := repos.AcquireUpstreamValidationLock(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, releaseC)
	releaseC()
}
