package billing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type repriceResolver struct {
	prices domain.ResolvedPrices
	ok     bool
	model  string
	tier   string
}

func repriceIntPtr(v int) *int { return &v }

func (r *repriceResolver) ResolvePrices(model string, _ int64, tier string, _ time.Time) (domain.ResolvedPrices, bool) {
	r.model, r.tier = model, tier
	return r.prices, r.ok
}

func TestRepriceUsageRestoresTierAndUpstreamEconomics(t *testing.T) {
	in, out := int64(1_000_000), int64(2_000_000)
	resolver := &repriceResolver{
		prices: domain.ResolvedPrices{InputPerM: &in, OutputPerM: &out},
		ok:     true,
	}
	row := domain.UnpricedUsage{
		ID: 7, Model: "requested", MappedModel: "actual", Format: domain.FormatOpenAIChat,
		BillingTier: "no_price:priority", InputTokens: 3, OutputTokens: 4, TotalTokens: 7,
		UpstreamMultiplierBP: repriceIntPtr(5_000), CreatedAt: time.Now(),
	}

	got, ok := RepriceUsage(row, resolver, 15_000)
	require.True(t, ok)
	require.Equal(t, "actual", resolver.model)
	require.Equal(t, "priority", resolver.tier)
	require.Equal(t, "priority", got.BillingTier)
	require.Equal(t, int64(11), got.RawCost)
	require.Equal(t, int64(17), got.Cost)
	require.Equal(t, int64(6), *got.UpstreamCost)
	require.Equal(t, int64(11), *got.GrossProfit)
	require.Equal(t, int64(6471), *got.ProfitMarginBP)
}

func TestRepriceUsageDoesNotSettleWithoutRequiredPrice(t *testing.T) {
	in := int64(1_000_000)
	resolver := &repriceResolver{
		prices: domain.ResolvedPrices{InputPerM: &in},
		ok:     true,
	}
	row := domain.UnpricedUsage{
		ID: 8, Model: "m", Format: domain.FormatOpenAIChat,
		InputTokens: 1, OutputTokens: 1,
	}
	_, ok := RepriceUsage(row, resolver, 10_000)
	require.False(t, ok)
}

func TestRepriceUsageResponsesCallOnlyNeedsImagePrice(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		resolver := &repriceResolver{prices: domain.ResolvedPrices{}, ok: true}
		row := domain.UnpricedUsage{ID: 9, Model: "responses", Format: domain.FormatOpenAIResponses, CallCount: 1}
		_, ok := RepriceUsage(row, resolver, 10_000)
		require.False(t, ok)
	})
	t.Run("priced", func(t *testing.T) {
		price := int64(5_400)
		resolver := &repriceResolver{prices: domain.ResolvedPrices{PricePerImage: &price}, ok: true}
		row := domain.UnpricedUsage{ID: 10, Model: "responses", Format: domain.FormatOpenAIResponses, CallCount: 2}
		got, ok := RepriceUsage(row, resolver, 10_000)
		require.True(t, ok)
		require.Equal(t, int64(10_800), got.RawCost)
		require.Equal(t, int64(10_800), got.Cost)
		require.Equal(t, price, *got.PricePerCallMillis)
	})
}

func TestRepriceUsageSearchUsesDefaultPriceWhenResolverHasNoCallRate(t *testing.T) {
	resolver := &repriceResolver{ok: false}
	row := domain.UnpricedUsage{ID: 11, Model: "search", Format: domain.FormatOpenAISearch, CallCount: 1}
	got, ok := RepriceUsage(row, resolver, 10_000)
	require.True(t, ok)
	require.Equal(t, domain.DefaultCodexSearchPricePerCall, *got.PricePerCallMillis)
	require.Equal(t, domain.DefaultCodexSearchPricePerCall, got.Cost)
}

func TestRepriceUsagePreservesPositiveChargeForSmallAmount(t *testing.T) {
	in, out := int64(1_000_000), int64(1_000_000)
	resolver := &repriceResolver{prices: domain.ResolvedPrices{InputPerM: &in, OutputPerM: &out}, ok: true}
	row := domain.UnpricedUsage{ID: 12, Model: "tiny", Format: domain.FormatOpenAIChat, InputTokens: 1, OutputTokens: 0}
	got, ok := RepriceUsage(row, resolver, 1)
	require.True(t, ok)
	require.Equal(t, int64(1), got.RawCost)
	require.Equal(t, int64(1), got.Cost)
}

var _ PriceResolver = (*repriceResolver)(nil)
