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

const summaryMaxInt64 = int64(1<<63 - 1)

// TestPGUsageSummarySaturatesInt64Aggregates verifies that valid bigint rows
// cannot make the admin financial summary fail when their numeric sum exceeds
// the API's int64 representation. Gross profit and margin are checked too:
// cost - upstream_cost is evaluated in numeric, and an extreme negative
// margin is clamped before the final bigint conversion.
func TestPGUsageSummarySaturatesInt64Aggregates(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	from, to := now.Add(-time.Minute), now.Add(time.Minute)

	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		{RequestID: "summary-cost-a", Model: "summary-cost", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, Cost: summaryMaxInt64, CreatedAt: now},
		{RequestID: "summary-cost-b", Model: "summary-cost", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, Cost: summaryMaxInt64, CreatedAt: now},
	}))

	upstream := summaryMaxInt64
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		{RequestID: "summary-upstream-a", Model: "summary-upstream", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, UpstreamCost: &upstream, CreatedAt: now},
		{RequestID: "summary-upstream-b", Model: "summary-upstream", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, UpstreamCost: &upstream, CreatedAt: now},
		{RequestID: "summary-margin", Model: "summary-margin", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, Cost: 1, UpstreamCost: &upstream, CreatedAt: now},
	}))

	query := func(model string) *repository.UsageLogsSummary {
		summary, err := repos.SummarizeUsages(ctx, repository.UsageQuery{Model: model, From: &from, To: &to})
		require.NoError(t, err)
		return summary
	}

	cost := query("summary-cost")
	require.Equal(t, int64(2), cost.RequestCount)
	require.Equal(t, summaryMaxInt64, cost.UserCharge, "sum(cost) must clamp at MaxInt64")
	require.Equal(t, int64(0), cost.AttributedUserCharge)
	require.Nil(t, cost.UpstreamCost)
	require.Nil(t, cost.GrossProfit)
	require.Nil(t, cost.ProfitMarginBP)

	up := query("summary-upstream")
	require.Equal(t, int64(2), up.RequestCount)
	require.Equal(t, int64(2), up.CostedRequestCount)
	require.NotNil(t, up.UpstreamCost)
	require.Equal(t, summaryMaxInt64, *up.UpstreamCost, "sum(upstream_cost) must clamp at MaxInt64")
	minInt64 := int64(-1 << 63)
	require.NotNil(t, up.GrossProfit)
	require.Equal(t, minInt64, *up.GrossProfit, "negative gross profit must clamp at MinInt64")
	require.Equal(t, int64(2), up.LossRequestCount)
	require.Nil(t, up.ProfitMarginBP, "zero attributed charge has no margin")

	margin := query("summary-margin")
	require.Equal(t, int64(1), margin.UserCharge)
	require.NotNil(t, margin.ProfitMarginBP)
	require.Equal(t, minInt64, *margin.ProfitMarginBP, "extreme negative margin must clamp before bigint scan")
}
