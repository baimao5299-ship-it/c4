package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type channelStatsFake struct {
	*fakeStore
	stats map[int64]*domain.PublicChannelStat
}

func (f *channelStatsFake) ScanPublicChannelStats(context.Context, []int64, time.Time, time.Time) (map[int64]*domain.PublicChannelStat, error) {
	return f.stats, nil
}

func TestUserChannelMetricsOnlyPublicAndComputesHealth(t *testing.T) {
	now := time.Now().UTC()
	store := &channelStatsFake{fakeStore: newFakeStore(), stats: map[int64]*domain.PublicChannelStat{
		1: {GroupID: 1, RequestCount: 20, ErrorCount: 1, LatencyTotalMS: 1800, LatencySampleCount: 2, LastCalledAt: ptrTime(now)},
		2: {GroupID: 2, RequestCount: 10, ErrorCount: 8, LatencyTotalMS: 40000, LatencySampleCount: 10, LastCalledAt: ptrTime(now.Add(-time.Minute))},
	}}
	store.groups[1] = &domain.Group{ID: 1, Name: "public", Visibility: domain.GroupVisibilityPublic, AllowedModels: []string{"gpt-5"}}
	store.groups[2] = &domain.Group{ID: 2, Name: "private", Visibility: domain.GroupVisibilityPrivate, AllowedModels: []string{"gpt-5"}}
	store.groups[3] = &domain.Group{ID: 3, Name: "degraded", Visibility: domain.GroupVisibilityPublic, AllowedModels: []string{"gpt-5"}}
	store.groups[4] = &domain.Group{ID: 4, Name: "empty", Visibility: domain.GroupVisibilityPublic, AllowedModels: []string{"gpt-5"}}
	store.stats[3] = &domain.PublicChannelStat{GroupID: 3, RequestCount: 10, ErrorCount: 8, LatencyTotalMS: 40000, LatencySampleCount: 10, LastCalledAt: ptrTime(now.Add(-time.Minute))}

	svc := &Service{store: store}
	rows, err := svc.UserChannelMetrics(context.Background(), 42, now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.Equal(t, int64(1), rows[0].Group.ID, "most recently called public group first")
	require.Equal(t, "stable", rows[0].Status)
	require.Equal(t, int64(900), rows[0].AverageLatencyMS)
	require.InDelta(t, 95, rows[0].SuccessRate, 0.001)
	require.Equal(t, "degraded", rows[1].Status)
	require.Equal(t, int64(3), rows[1].Group.ID)
	require.Equal(t, "no_data", rows[2].Status)
}

func TestUserChannelMetricsIncludesModelPricesAndKeepsUnpricedModels(t *testing.T) {
	now := time.Now().UTC()
	store := &channelStatsFake{fakeStore: newFakeStore(), stats: map[int64]*domain.PublicChannelStat{}}
	store.groups[1] = &domain.Group{
		ID: 1, Name: "priced", Visibility: domain.GroupVisibilityPublic,
		AllowedModels: []string{"gpt-priced", "gpt-unpriced"}, PriceMultiplier: 800,
	}
	inPrice, outPrice := int64(100000), int64(250000)
	_, err := store.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{{
		Model: "gpt-priced", Mode: domain.PriceModeToken,
		InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceLitellm,
	}})
	require.NoError(t, err)

	svc := &Service{store: store}
	rows, err := svc.UserChannelMetrics(context.Background(), 42, now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].ModelPrices, 2, "unpriced models stay visible")
	require.Equal(t, "gpt-priced", rows[0].ModelPrices[0].Model)
	require.Equal(t, int64(100000), *rows[0].ModelPrices[0].InputPerM)
	require.Equal(t, int64(250000), *rows[0].ModelPrices[0].OutputPerM)
	require.Equal(t, "gpt-unpriced", rows[0].ModelPrices[1].Model)
	require.Nil(t, rows[0].ModelPrices[1].InputPerM)
	require.Nil(t, rows[0].ModelPrices[1].OutputPerM)
}

func ptrTime(t time.Time) *time.Time { return &t }
