// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestMarkLastUsedPreservesRuleState(t *testing.T) {
	cooldown := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	lastError := "rate limited"
	a := &accountSnapshot{}
	a.state.Store(&accState{
		status:        domain.Status429,
		cooldownUntil: &cooldown,
		errCount:      4,
		lastError:     &lastError,
	})

	used := cooldown.Add(time.Second)
	markLastUsed(a, used)

	got := a.state.Load()
	require.Equal(t, domain.Status429, got.status)
	require.Equal(t, cooldown, *got.cooldownUntil)
	require.Equal(t, 4, got.errCount)
	require.Equal(t, lastError, *got.lastError)
	require.Equal(t, used, *got.lastUsedAt)
}
