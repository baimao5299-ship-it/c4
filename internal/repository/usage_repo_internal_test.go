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

func TestUsageLogRankFiltersQualifyColumnsAndPreserveWindow(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	where, args := usageLogRankFilters(UsageQuery{GroupID: 7, Model: "gpt-latest", From: &from, To: &to})
	require.Equal(t, []any{int64(7), "gpt-latest", from, to}, args)
	require.Contains(t, where, "l.group_id = $1")
	require.Contains(t, where, "l.model = $2")
	require.Contains(t, where, "l.created_at >= $3")
	require.Contains(t, where, "l.created_at < $4")
}
