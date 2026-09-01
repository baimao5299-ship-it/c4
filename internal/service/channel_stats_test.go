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
	require.Equal(t, int64(100000), *rows[0].ModelPrices[0].OfficialInputPerM,
		"official input price comes from the catalogue row")
	require.Equal(t, int64(250000), *rows[0].ModelPrices[0].OfficialOutputPerM,
		"official output price comes from the catalogue row")
	require.Equal(t, "gpt-unpriced", rows[0].ModelPrices[1].Model)
	require.Nil(t, rows[0].ModelPrices[1].InputPerM)
	require.Nil(t, rows[0].ModelPrices[1].OutputPerM)
	require.Nil(t, rows[0].ModelPrices[1].OfficialInputPerM)
	require.Nil(t, rows[0].ModelPrices[1].OfficialOutputPerM)
}

func TestUserChannelMetricsSeparatesOfficialAndResolvedPrices(t *testing.T) {
	now := time.Now().UTC()
	store := &channelStatsFake{fakeStore: newFakeStore(), stats: map[int64]*domain.PublicChannelStat{}}
	store.groups[1] = &domain.Group{
		ID: 1, Name: "discounted", Visibility: domain.GroupVisibilityPublic,
		AllowedModels: []string{"gpt-discounted"}, PriceMultiplier: 800,
	}
	inPrice, outPrice := int64(100000), int64(250000)
	store.priceEntries["gpt-discounted"] = &domain.PriceEntry{
		Model: "gpt-discounted", Mode: domain.PriceModeToken,
		InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceLitellm,
	}
	variantMultiplier := 5000
	store.priceVariants["gpt-discounted"] = []*domain.PriceVariant{{
		Model: "gpt-discounted", Seq: 1, MultBP: &variantMultiplier,
	}}

	svc := &Service{store: store}
	rows, err := svc.UserChannelMetrics(context.Background(), 42, now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].ModelPrices, 1)
	price := rows[0].ModelPrices[0]
	require.Equal(t, int64(100000), *price.OfficialInputPerM,
		"official price ignores conditional and group multipliers")
	require.Equal(t, int64(250000), *price.OfficialOutputPerM)
	require.Equal(t, int64(50000), *price.InputPerM,
		"resolved price includes the matching conditional variant")
	require.Equal(t, int64(125000), *price.OutputPerM)
}

func TestUserChannelMetricsKeepsCataloguePriceForInvalidRuntimeVariant(t *testing.T) {
	now := time.Now().UTC()
	store := &channelStatsFake{fakeStore: newFakeStore(), stats: map[int64]*domain.PublicChannelStat{}}
	store.groups[1] = &domain.Group{
		ID: 1, Name: "catalogue-only", Visibility: domain.GroupVisibilityPublic,
		AllowedModels: []string{"gpt-invalid-variant"}, PriceMultiplier: 800,
	}
	inPrice, outPrice := int64(100), int64(250)
	store.priceEntries["gpt-invalid-variant"] = &domain.PriceEntry{
		Model: "gpt-invalid-variant", Mode: domain.PriceModeToken,
		InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceLitellm,
	}
	// This branch cannot be represented on the persisted 1e-5 USD grid. It is
	// excluded from runtime billing, but the base catalogue row remains useful
	// to explain the model's official price in the public monitor.
	variantMultiplier := 10
	store.priceVariants["gpt-invalid-variant"] = []*domain.PriceVariant{{
		Model: "gpt-invalid-variant", Seq: 1, MultBP: &variantMultiplier,
	}}

	svc := &Service{store: store}
	rows, err := svc.UserChannelMetrics(context.Background(), 42, now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].ModelPrices, 1)
	price := rows[0].ModelPrices[0]
	require.Nil(t, price.InputPerM, "invalid runtime branch must not look billable")
	require.Nil(t, price.OutputPerM)
	require.Equal(t, int64(100), *price.OfficialInputPerM)
	require.Equal(t, int64(250), *price.OfficialOutputPerM)
}

func ptrTime(t time.Time) *time.Time { return &t }
