// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReleaseUpstreamValidationLockClosesConnectionWhenUnlockFails(t *testing.T) {
	unlockErr := errors.New("unlock failed")
	var closeCalls, releaseCalls int

	releaseAdvisoryLock(
		func(context.Context) error { return unlockErr },
		func(context.Context) error {
			closeCalls++
			return nil
		},
		func() { releaseCalls++ },
	)

	require.Equal(t, 1, closeCalls)
	require.Equal(t, 1, releaseCalls)
}

func TestReleaseUpstreamValidationLockKeepsHealthyConnectionOnSuccess(t *testing.T) {
	var closeCalls, releaseCalls int

	releaseAdvisoryLock(
		func(context.Context) error { return nil },
		func(context.Context) error {
			closeCalls++
			return nil
		},
		func() { releaseCalls++ },
	)

	require.Zero(t, closeCalls)
	require.Equal(t, 1, releaseCalls)
}
