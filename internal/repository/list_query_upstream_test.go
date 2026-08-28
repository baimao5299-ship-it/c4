// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamSortWhitelistMatchesOpenAPI(t *testing.T) {
	for _, field := range []string{"id", "name", "base_url", "multiplier_bp", "request_count", "success_count", "failure_count", "last_checked_at", "created_at", "updated_at"} {
		_, err := (ListQuery{Sort: field, Order: "asc"}).sortOrder(upstreamSortFields)
		require.NoError(t, err, field)
	}
	_, err := (ListQuery{Sort: "not-a-field"}).sortOrder(upstreamSortFields)
	require.ErrorIs(t, err, ErrInvalidSort)
}
