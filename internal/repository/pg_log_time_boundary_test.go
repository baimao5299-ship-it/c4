// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestPGLogQueriesUseHalfOpenTimeWindow locks the shared [from,to) contract
// for detail queries and the financial summary. A row exactly at To must be
// absent from all three views, otherwise adjacent windows double-count it.
func TestPGLogQueriesUseHalfOpenTimeWindow(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	// PostgreSQL timestamptz stores microseconds; truncate query and seed times
	// alike so the row at To is exactly equal after the round trip.
	now := time.Now().UTC().Truncate(time.Microsecond)
	from := now.Add(-2 * time.Minute)
	to := now.Add(2 * time.Minute)
	inside := now

	usageRows := []*domain.UsageLog{
		{RequestID: "half-open-from", Model: "half-open", Format: domain.FormatOpenAIChat,
			ErrorType: domain.ErrNone, Cost: 11, CreatedAt: from},
		{RequestID: "half-open-inside", Model: "half-open", Format: domain.FormatOpenAIChat,
			ErrorType: domain.ErrNone, Cost: 13, CreatedAt: inside},
		{RequestID: "half-open-to", Model: "half-open", Format: domain.FormatOpenAIChat,
			ErrorType: domain.ErrNone, Cost: 17, CreatedAt: to},
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, usageRows))

	errRows := []*domain.UsageLog{
		{RequestID: "half-open-err-from", Model: "half-open-err", Format: domain.FormatOpenAIChat,
			StatusCode: 429, ErrorType: domain.Err429, CreatedAt: from},
		{RequestID: "half-open-err-inside", Model: "half-open-err", Format: domain.FormatOpenAIChat,
			StatusCode: 429, ErrorType: domain.Err429, CreatedAt: inside},
		{RequestID: "half-open-err-to", Model: "half-open-err", Format: domain.FormatOpenAIChat,
			StatusCode: 429, ErrorType: domain.Err429, CreatedAt: to},
	}
	require.NoError(t, repos.InsertErrLogBatch(ctx, errRows))

	gotUsage, err := repos.QueryUsages(ctx, repository.UsageQuery{
		Model: "half-open", From: &from, To: &to, Limit: 100,
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"half-open-from", "half-open-inside"}, requestIDs(gotUsage))

	gotErr, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{
		Model: "half-open-err", From: &from, To: &to, Limit: 100,
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"half-open-err-from", "half-open-err-inside"}, requestIDs(gotErr))

	summary, err := repos.SummarizeUsages(ctx, repository.UsageQuery{
		Model: "half-open", From: &from, To: &to,
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), summary.RequestCount)
	require.Equal(t, int64(24), summary.UserCharge, "To boundary row must not enter summary")
}

func requestIDs(rows []*domain.UsageLog) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.RequestID)
	}
	return ids
}
