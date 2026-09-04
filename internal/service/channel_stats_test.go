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
	first, second, third := int64(1), int64(2), int64(3)
	store := &channelStatsFake{fakeStore: newFakeStore(), stats: map[int64]*domain.PublicChannelStat{
		1: {GroupID: 1, RequestCount: 20, ErrorCount: 1, LatencyTotalMS: 1800, LatencySampleCount: 2, LastCalledAt: ptrTime(now)},
		2: {GroupID: 2, RequestCount: 10, ErrorCount: 8, LatencyTotalMS: 40000, LatencySampleCount: 10, LastCalledAt: ptrTime(now.Add(-time.Minute))},
	}}
	store.groups[1] = &domain.Group{ID: 1, Name: "public", DisplayOrder: &first, Visibility: domain.GroupVisibilityPublic, AllowedModels: []string{"gpt-5"}}
	store.groups[2] = &domain.Group{ID: 2, Name: "private", Visibility: domain.GroupVisibilityPrivate, AllowedModels: []string{"gpt-5"}}
	store.groups[3] = &domain.Group{ID: 3, Name: "degraded", DisplayOrder: &second, Visibility: domain.GroupVisibilityPublic, AllowedModels: []string{"gpt-5"}}
	store.groups[4] = &domain.Group{ID: 4, Name: "empty", DisplayOrder: &third, Visibility: domain.GroupVisibilityPublic, AllowedModels: []string{"gpt-5"}}
	store.stats[3] = &domain.PublicChannelStat{GroupID: 3, RequestCount: 10, ErrorCount: 8, LatencyTotalMS: 40000, LatencySampleCount: 10, LastCalledAt: ptrTime(now.Add(-time.Minute))}

	svc := &Service{store: store}
	rows, err := svc.UserChannelMetrics(context.Background(), 42, now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.Equal(t, int64(1), rows[0].Group.ID, "administrator display order wins over recent activity")
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

func TestUserChannelMetricsDeduplicatesLegacyModelSpellings(t *testing.T) {
	now := time.Now().UTC()
	store := &channelStatsFake{fakeStore: newFakeStore(), stats: map[int64]*domain.PublicChannelStat{}}
	store.groups[1] = &domain.Group{
		ID: 1, Name: "legacy aliases", Visibility: domain.GroupVisibilityPublic,
		AllowedModels: []string{"Claude-Fable-5.1", "claude-fable-5-1", " CLAUDE_FABLE_5_1 ", "gpt-5"},
	}
	svc := &Service{store: store}
	rows, err := svc.UserChannelMetrics(context.Background(), 42, now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, []string{"Claude-Fable-5.1", "gpt-5"}, rows[0].Group.AllowedModels)
	require.Len(t, rows[0].ModelPrices, 2)
	require.Equal(t, "Claude-Fable-5.1", rows[0].ModelPrices[0].Model)
}

// Two spellings that differ only by punctuation are NOT automatically the same
// model. When they carry different prices they are different products, so both
// must stay visible; collapsing them would hide one model and bill the user at
// the other one's price.
func TestUserChannelMetricsKeepsSeparatelyPricedPunctuationVariants(t *testing.T) {
	now := time.Now().UTC()
	store := &channelStatsFake{fakeStore: newFakeStore(), stats: map[int64]*domain.PublicChannelStat{}}
	store.groups[1] = &domain.Group{
		ID: 1, Name: "variants", Visibility: domain.GroupVisibilityPublic,
		AllowedModels: []string{"deepseek-v3.2", "deepseek.v3.2"},
	}
	dashIn, dashOut := int64(100000), int64(200000)
	dotIn, dotOut := int64(700000), int64(900000)
	_, err := store.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "deepseek-v3.2", Mode: domain.PriceModeToken, InputPerM: &dashIn, OutputPerM: &dashOut, Source: domain.PricingSourceLitellm},
		{Model: "deepseek.v3.2", Mode: domain.PriceModeToken, InputPerM: &dotIn, OutputPerM: &dotOut, Source: domain.PricingSourceLitellm},
	})
	require.NoError(t, err)

	svc := &Service{store: store}
	rows, err := svc.UserChannelMetrics(context.Background(), 42, now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].ModelPrices, 2, "differently priced spellings must both stay visible")
	require.Equal(t, []string{"deepseek-v3.2", "deepseek.v3.2"}, rows[0].Group.AllowedModels)
	byModel := make(map[string]PublicChannelModelPrice, 2)
	for _, row := range rows[0].ModelPrices {
		byModel[row.Model] = row
	}
	require.Equal(t, int64(100000), *byModel["deepseek-v3.2"].InputPerM)
	require.Equal(t, int64(700000), *byModel["deepseek.v3.2"].InputPerM,
		"each spelling keeps its own price instead of inheriting the other's")
}

func TestUserChannelMetricsResolvesRelayModelAliases(t *testing.T) {
	now := time.Now().UTC()
	store := &channelStatsFake{fakeStore: newFakeStore(), stats: map[int64]*domain.PublicChannelStat{}}
	requested := []string{"k3", "Claude-Fable-5.1", "Claude-Opus-5"}
	store.groups[1] = &domain.Group{
		ID: 1, Name: "relay aliases", Visibility: domain.GroupVisibilityPublic,
		AllowedModels: requested, PriceMultiplier: 6500,
	}
	for model, pair := range map[string][2]int64{
		"kimi-k3":          {300000, 1500000},
		"claude-fable-5-1": {1000000, 5000000},
		"claude-opus-5":    {500000, 2500000},
	} {
		input, output := pair[0], pair[1]
		_, err := store.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{{
			Model: model, Mode: domain.PriceModeToken,
			InputPerM: &input, OutputPerM: &output, Source: domain.PricingSourceLitellm,
		}})
		require.NoError(t, err)
	}

	svc := &Service{store: store}
	rows, err := svc.UserChannelMetrics(context.Background(), 42, now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].ModelPrices, len(requested))
	for i, model := range requested {
		price := rows[0].ModelPrices[i]
		require.Equal(t, model, price.Model)
		require.NotNilf(t, price.InputPerM, "%s should have a resolved input price", model)
		require.NotNilf(t, price.OutputPerM, "%s should have a resolved output price", model)
		require.NotNilf(t, price.OfficialInputPerM, "%s should have an official input price", model)
		require.NotNilf(t, price.OfficialOutputPerM, "%s should have an official output price", model)
	}
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

func TestUserChannelMetricsIncludesImageTokenPrices(t *testing.T) {
	now := time.Now().UTC()
	store := &channelStatsFake{fakeStore: newFakeStore(), stats: map[int64]*domain.PublicChannelStat{}}
	store.groups[1] = &domain.Group{
		ID: 1, Name: "image", Visibility: domain.GroupVisibilityPublic,
		AllowedModels: []string{"image-model"}, PriceMultiplier: 800,
	}
	inPrice, outPrice := int64(120000), int64(340000)
	store.priceEntries["image-model"] = &domain.PriceEntry{
		Model: "image-model", Mode: domain.PriceModeImage,
		ImgInTokPerM: &inPrice, ImgOutTokPerM: &outPrice,
		Source: domain.PricingSourceLitellm,
	}

	svc := &Service{store: store}
	rows, err := svc.UserChannelMetrics(context.Background(), 42, now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].ModelPrices, 1)
	price := rows[0].ModelPrices[0]
	require.Equal(t, int64(120000), *price.ImgInTokPerM)
	require.Equal(t, int64(340000), *price.ImgOutTokPerM)
	require.Equal(t, int64(120000), *price.OfficialImgInTokPerM)
	require.Equal(t, int64(340000), *price.OfficialImgOutTokPerM)
}

func ptrTime(t time.Time) *time.Time { return &t }
