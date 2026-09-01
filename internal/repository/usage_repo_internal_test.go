// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUsageLogsSummarySQLUsesHalfOpenUpperBound(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	query, args := usageLogsSummarySQL(UsageQuery{From: &from, To: &to})

	require.Len(t, args, 2)
	require.Contains(t, query, "created_at >= $1")
	require.Contains(t, query, "created_at < $2")
	require.NotContains(t, strings.ToLower(query), "created_at <=")
}
