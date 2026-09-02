package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestBuildLogRaisesUnderReportedTotalToMeasuredComponents(t *testing.T) {
	l := (&Proxy{}).buildLog("r", 1, 1, "m", "m", domain.FormatOpenAIChat, 200,
		domain.ErrNone, usageTuple{it: 80, ot: 20, tt: 7, cr: 10}, time.Now())
	// OpenAI input is already cache-normalized in normal extraction. The
	// provider total still includes cached input, so quota must never fall below
	// the measured billable input + output + cache-read components.
	require.Equal(t, int64(110), l.TotalTokens)
}

func TestPricingPromptTokensRestoresOpenAICacheRead(t *testing.T) {
	l := &domain.UsageLog{Format: domain.FormatOpenAIChat, InputTokens: 90, CacheReadTokens: 10}
	require.Equal(t, int64(100), pricingPromptTokens(l))
	l.Format = domain.FormatAnthropic
	require.Equal(t, int64(90), pricingPromptTokens(l), "Anthropic input_tokens excludes cache_read")
}

func TestLogWithCtxFillsPartialProviderUsage(t *testing.T) {
	rm := &reqMeta{billingEnabled: true, estimatedInputTokens: 40}
	ctx := context.WithValue(context.Background(), ctxKeyReqMeta{}, rm)
	l := &domain.UsageLog{Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, OutputTokens: 6, TotalTokens: 6}
	got := logWithCtx(ctx, l)
	require.Equal(t, int64(40), got.InputTokens)
	require.Equal(t, int64(6), got.OutputTokens)
	require.Equal(t, int64(46), got.TotalTokens)
}

func TestLogWithCtxEstimatesMissingUsageOnce(t *testing.T) {
	rm := &reqMeta{billingEnabled: true, estimatedInputTokens: 40}
	ctx := context.WithValue(context.Background(), ctxKeyReqMeta{}, rm)
	l := &domain.UsageLog{Format: domain.FormatAnthropic, ErrorType: domain.ErrNone}
	got := logWithCtx(ctx, l)
	require.Equal(t, int64(40), got.InputTokens)
	require.Equal(t, int64(1), got.OutputTokens)
	require.Equal(t, int64(41), got.TotalTokens)
}

func TestLogWithCtxCacheEstimateAvoidsDoubleCounting(t *testing.T) {
	rm := &reqMeta{billingEnabled: true, estimatedInputTokens: 40}
	ctx := context.WithValue(context.Background(), ctxKeyReqMeta{}, rm)
	l := &domain.UsageLog{Format: domain.FormatOpenAIResponses, ErrorType: domain.ErrNone, CacheReadTokens: 10}
	got := logWithCtx(ctx, l)
	require.Equal(t, int64(30), got.InputTokens)
	require.Equal(t, int64(1), got.OutputTokens)
	require.Equal(t, int64(41), got.TotalTokens)
}

func TestLogWithCtxAnthropicCacheEstimateIsNotSubtracted(t *testing.T) {
	rm := &reqMeta{billingEnabled: true, estimatedInputTokens: 40}
	ctx := context.WithValue(context.Background(), ctxKeyReqMeta{}, rm)
	l := &domain.UsageLog{Format: domain.FormatAnthropic, ErrorType: domain.ErrNone, CacheReadTokens: 10}
	got := logWithCtx(ctx, l)
	// Anthropic input_tokens excludes cache_read_input_tokens, so the local
	// input estimate must remain intact and cache reads are charged separately.
	require.Equal(t, int64(40), got.InputTokens)
	require.Equal(t, int64(1), got.OutputTokens)
	require.Equal(t, int64(41), got.TotalTokens)
}

func TestSettlementPricesRequireObservedCacheRates(t *testing.T) {
	in, out := int64(10), int64(20)
	base := domain.ResolvedPrices{InputPerM: &in, OutputPerM: &out}
	l := &domain.UsageLog{Format: domain.FormatOpenAIChat, InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
	require.True(t, settlementPricesUsable(l, base))
	l.CacheReadTokens = 1
	require.True(t, settlementPricesUsable(l, base), "missing cache rate is settled with the conservative input-rate fallback")
	cache := int64(3)
	base.CacheReadPerM = &cache
	require.True(t, settlementPricesUsable(l, base))
}

func TestObservedCacheRateFallbackIsLogged(t *testing.T) {
	in := int64(123)
	l := &domain.UsageLog{CacheReadTokens: 2, CacheCreationTokens: 3}
	rp := withObservedCacheRateFallback(l, domain.ResolvedPrices{InputPerM: &in})
	require.NotNil(t, rp.CacheReadPerM)
	require.NotNil(t, rp.CacheWritePerM)
	require.Equal(t, in, *rp.CacheReadPerM)
	require.Equal(t, in, *rp.CacheWritePerM)
}

func TestNegativePricesAreNotAdmittedToSettlement(t *testing.T) {
	neg, out := int64(-1), int64(10)
	rp := domain.ResolvedPrices{InputPerM: &neg, OutputPerM: &out}
	require.False(t, tokenPricesComplete(rp))
	require.False(t, resolvedPricesUsable(domain.FormatOpenAIChat, rp))
	imageNeg := domain.ResolvedPrices{PricePerImage: &neg}
	require.False(t, resolvedPricesUsable(domain.FormatOpenAIImages, imageNeg))
	callNeg := domain.ResolvedPrices{PricePerCall: &neg}
	require.False(t, settlementPricesUsable(&domain.UsageLog{Format: domain.FormatOpenAISearch, CallCount: 1}, callNeg))
}

func TestImageSettlementAcceptsTokenOnlyUsage(t *testing.T) {
	in, out := int64(100), int64(200)
	l := &domain.UsageLog{Format: domain.FormatOpenAIImages, InputTokens: 12, OutputTokens: 8}
	rp := domain.ResolvedPrices{Mode: domain.PriceModeImage, ImgInTokPerM: &in, ImgOutTokPerM: &out}
	require.True(t, settlementPricesUsable(l, rp), "image-token-only providers must not be rejected when both observed token components are priced")
}

func TestImageSettlementRequiresPriceForObservedComponent(t *testing.T) {
	in, out := int64(100), int64(200)
	rp := domain.ResolvedPrices{Mode: domain.PriceModeImage, ImgInTokPerM: &in, ImgOutTokPerM: &out}
	require.False(t, settlementPricesUsable(&domain.UsageLog{Format: domain.FormatOpenAIImages, InputTokens: 12}, domain.ResolvedPrices{Mode: domain.PriceModeImage, ImgOutTokPerM: &out}), "observed image input tokens cannot settle without an input rate")
	require.False(t, settlementPricesUsable(&domain.UsageLog{Format: domain.FormatOpenAIImages, CallCount: 1}, rp), "count-only image usage requires a per-image rate")
}

func TestImageSettlementRejectsObservedTokensWithoutAnyRate(t *testing.T) {
	l := &domain.UsageLog{Format: domain.FormatOpenAIImages, InputTokens: 4, OutputTokens: 6}
	require.False(t, settlementPricesUsable(l, domain.ResolvedPrices{Mode: domain.PriceModeImage}), "observed image tokens must not settle as a zero-cost request")
}
