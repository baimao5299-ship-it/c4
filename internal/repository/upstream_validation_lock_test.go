// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

func TestRepositoryValidationLockWithoutPoolFallsBackExplicitly(t *testing.T) {
	var repos repository.Repository
	release, ok, err := repos.AcquireUpstreamValidationLock(context.Background())
	require.ErrorIs(t, err, repository.ErrUpstreamValidationLockUnavailable)
	require.False(t, ok)
	require.Nil(t, release)
}
