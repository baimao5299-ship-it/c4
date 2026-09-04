// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/ent"
)

func TestToDomainUpstreamPreservesRetainedModelsWithoutCompleteStamp(t *testing.T) {
	row := &ent.Upstream{
		ID:     50,
		Name:   "relay",
		Models: []string{"model-a", "model-b"},
	}

	got := toDomainUpstream(row)

	require.Equal(t, []string{"model-a", "model-b"}, got.Models)
	require.Nil(t, got.ModelsCheckedAt)
}
