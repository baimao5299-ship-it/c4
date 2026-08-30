// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReleaseAdvisoryLockClosesConnectionWhenUnlockFails(t *testing.T) {
	unlockErr := errors.New("unlock failed")
	var events []string
	var mu sync.Mutex
	appendEvent := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}

	releaseAdvisoryLock(
		func(ctx context.Context) error {
			requireDeadline(t, ctx)
			appendEvent("unlock")
			return unlockErr
		},
		func(ctx context.Context) error {
			requireDeadline(t, ctx)
			appendEvent("close")
			return nil
		},
		func() { appendEvent("release") },
	)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"unlock", "close", "release"}, events)
}

func TestReleaseAdvisoryLockKeepsConnectionOnSuccess(t *testing.T) {
	var closeCalls, releaseCalls int
	releaseAdvisoryLock(
		func(ctx context.Context) error {
			requireDeadline(t, ctx)
			return nil
		},
		func(context.Context) error {
			closeCalls++
			return nil
		},
		func() { releaseCalls++ },
	)
	require.Zero(t, closeCalls)
	require.Equal(t, 1, releaseCalls)
}

func TestReleaseAdvisoryLockUnlockTimeoutIsBounded(t *testing.T) {
	releaseAdvisoryLock(
		func(ctx context.Context) error {
			requireDeadline(t, ctx)
			return context.DeadlineExceeded
		},
		func(context.Context) error { return nil },
		func() {},
	)
}

func requireDeadline(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	require.True(t, ok, "release callback must receive a bounded context")
	require.True(t, deadline.After(time.Now()), "release callback deadline must be in the future")
	require.LessOrEqual(t, time.Until(deadline), advisoryLockReleaseTimeout)
}
