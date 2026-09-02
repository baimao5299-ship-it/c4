// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
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

func TestCloneUpstreamModelFormatsDeepCopiesSlices(t *testing.T) {
	in := map[string][]domain.RequestFormat{
		"chat-only": {domain.FormatOpenAIChat},
	}
	got := cloneUpstreamModelFormats(in)

	in["chat-only"][0] = domain.FormatAnthropic
	in["new"] = []domain.RequestFormat{domain.FormatOpenAIResponses}

	require.Equal(t, []domain.RequestFormat{domain.FormatOpenAIChat}, got["chat-only"])
	require.NotContains(t, got, "new")
	require.NotNil(t, cloneUpstreamModelFormats(nil))
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
