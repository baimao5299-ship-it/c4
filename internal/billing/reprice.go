// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"context"
	"math/bits"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// recoveryTier decodes the marker written by the proxy. Older rows use plain
// no_price and therefore recover with the default tier; newer rows may retain a
// non-default service tier after the marker.
func recoveryTier(raw string) string {
	const prefix = "no_price:"
	if strings.HasPrefix(raw, prefix) {
		if tier := strings.TrimSpace(strings.TrimPrefix(raw, prefix)); tier != "" {
			return tier
		}
	}
	return "auto"
}

func nonNegativePrice(v *int64) bool { return v != nil && *v >= 0 }

func pricingPromptTokens(u domain.UnpricedUsage) int64 {
	prompt := u.InputTokens
	switch u.Format {
	case domain.FormatOpenAIChat, domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS:
		if u.CacheReadTokens > 0 && prompt <= (1<<63-1)-u.CacheReadTokens {
			prompt += u.CacheReadTokens
		}
	}
	return prompt
}

func resolvedPricesUsable(u domain.UnpricedUsage, rp domain.ResolvedPrices) bool {
	switch u.Format {
	case domain.FormatOpenAIImages:
		hasTokenPrices := rp.ImgInTokPerM != nil || rp.ImgOutTokPerM != nil
		if hasTokenPrices {
			if u.InputTokens > 0 && !nonNegativePrice(rp.ImgInTokPerM) {
				return false
			}
			if u.OutputTokens > 0 && !nonNegativePrice(rp.ImgOutTokPerM) {
				return false
			}
		} else if (u.InputTokens > 0 || u.OutputTokens > 0) && !nonNegativePrice(rp.PricePerImage) {
			return false
		}
		if u.CallCount > 0 && !nonNegativePrice(rp.PricePerImage) && u.InputTokens == 0 && u.OutputTokens == 0 {
			return false
		}
		return true
	case domain.FormatOpenAISearch:
		return u.CallCount <= 0 || nonNegativePrice(rp.PricePerCall)
	default:
		// A Responses image/tool call is a separate billable component. A token
		// snapshot alone must not settle the row and silently lose the call
		// charge; wait until the per-image price is available.
		if u.CallCount > 0 &&
			(u.Format == domain.FormatOpenAIResponses || u.Format == domain.FormatOpenAIResponsesWS) &&
			!nonNegativePrice(rp.PricePerImage) {
			return false
		}
		if u.InputTokens > 0 || u.OutputTokens > 0 || u.TotalTokens > 0 {
			return nonNegativePrice(rp.InputPerM) && nonNegativePrice(rp.OutputPerM)
		}
		if u.CallCount > 0 && (u.Format == domain.FormatOpenAIResponses || u.Format == domain.FormatOpenAIResponsesWS) {
			return nonNegativePrice(rp.PricePerImage)
		}
		return true
	}
}

func withCacheFallback(u domain.UnpricedUsage, rp domain.ResolvedPrices) domain.ResolvedPrices {
	if rp.InputPerM == nil {
		return rp
	}
	if u.CacheReadTokens > 0 && rp.CacheReadPerM == nil {
		v := *rp.InputPerM
		rp.CacheReadPerM = &v
	}
	if u.CacheCreationTokens > 0 && rp.CacheWritePerM == nil {
		v := *rp.InputPerM
		rp.CacheWritePerM = &v
	}
	return rp
}

// RepriceUsage resolves one persisted no_price row and computes all ledger
// fields without side effects. The caller applies the returned row in a single
// database transaction.
func RepriceUsage(u domain.UnpricedUsage, resolver PriceResolver, multiplier int) (domain.RepricedUsage, bool) {
	model := strings.TrimSpace(u.MappedModel)
	if model == "" {
		model = strings.TrimSpace(u.Model)
	}
	if model == "" || resolver == nil {
		return domain.RepricedUsage{}, false
	}
	tier := recoveryTier(u.BillingTier)
	rp, ok := resolver.ResolvePrices(model, pricingPromptTokens(u), tier, u.CreatedAt)
	if u.Format == domain.FormatOpenAISearch && (!ok || !nonNegativePrice(rp.PricePerCall)) {
		v := domain.DefaultCodexSearchPricePerCall
		rp.PricePerCall = &v
		ok = true
	}
	if !ok || !resolvedPricesUsable(u, rp) {
		return domain.RepricedUsage{}, false
	}
	var parts CostParts
	result := domain.RepricedUsage{ID: u.ID, BillingTier: tier}
	switch u.Format {
	case domain.FormatOpenAIImages:
		if rp.ImgInTokPerM != nil {
			v := *rp.ImgInTokPerM
			result.PriceInputMillis = &v
		}
		if rp.ImgOutTokPerM != nil {
			v := *rp.ImgOutTokPerM
			result.PriceOutputMillis = &v
		}
		if rp.PricePerImage != nil && u.CallCount > 0 {
			v := *rp.PricePerImage
			result.PricePerCallMillis = &v
		}
		parts = ImageCostPartsFromResolved(rp, u.InputTokens, u.OutputTokens, u.CallCount)
	case domain.FormatOpenAISearch:
		if rp.PricePerCall != nil {
			v := *rp.PricePerCall
			result.PricePerCallMillis = &v
		}
		parts = CostParts{Units: CallCostFromResolved(rp, u.CallCount)}
	default:
		rp = withCacheFallback(u, rp)
		if rp.InputPerM != nil {
			v := *rp.InputPerM
			result.PriceInputMillis = &v
		}
		if rp.OutputPerM != nil {
			v := *rp.OutputPerM
			result.PriceOutputMillis = &v
		}
		if u.CacheReadTokens > 0 && rp.CacheReadPerM != nil {
			v := *rp.CacheReadPerM
			result.PriceCacheReadMillis = &v
		}
		if u.CacheCreationTokens > 0 && rp.CacheWritePerM != nil {
			v := *rp.CacheWritePerM
			result.PriceCacheCreationMillis = &v
		}
		if u.CallCount > 0 && rp.PricePerImage != nil {
			v := *rp.PricePerImage
			result.PricePerCallMillis = &v
			parts = AddCostParts(parts, ImageCostPartsFromResolved(rp, 0, 0, u.CallCount))
		}
		parts = AddCostParts(parts, CostPartsFromResolved(rp, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens))
	}
	result.RawCost = parts.Rounded()
	result.Cost = ApplyMultiplier(result.RawCost, multiplier)
	// A recovered request with a positive raw amount must never become a
	// permanently unbilled zero after rounding. Free groups (multiplier=0) keep
	// their deliberate zero charge and are absorbed by the normal zero-cost lane.
	if result.RawCost > 0 && result.Cost == 0 && multiplier > 0 {
		result.Cost = 1
	}
	if u.UpstreamMultiplierBP != nil {
		upstream := ApplyUpstreamMultiplier(result.RawCost, *u.UpstreamMultiplierBP)
		result.UpstreamCost = &upstream
		profit := result.Cost - upstream
		result.GrossProfit = &profit
		if result.Cost > 0 {
			margin := signedMulDivBP(profit, result.Cost)
			result.ProfitMarginBP = &margin
		}
	}
	return result, true
}

func signedMulDivBP(value, divisor int64) int64 {
	if value == 0 || divisor <= 0 {
		return 0
	}
	negative := value < 0
	magnitude := uint64(value)
	if negative {
		magnitude = uint64(-(value + 1)) + 1
	}
	hi, lo := bits.Mul64(magnitude, 10000)
	denominator := uint64(divisor)
	if hi >= denominator {
		if negative {
			return -1<<63 + 1
		}
		return 1<<63 - 1
	}
	quotient, remainder := bits.Div64(hi, lo, denominator)
	if remainder >= (denominator+1)/2 {
		quotient++
	}
	if quotient > uint64(1<<63-1) {
		if negative {
			return -1<<63 + 1
		}
		return 1<<63 - 1
	}
	if negative {
		return -int64(quotient)
	}
	return int64(quotient)
}

// repriceNoPrice resolves and atomically persists a bounded batch before the
// normal settlement lanes run. Rows that still have no usable price stay in
// the ledger for a later pricing refresh; they are never marked billed as if
// they were free.
func (f *Flusher) repriceNoPrice(ctx context.Context) int64 {
	if f == nil || f.unpriced == nil || f.resolver == nil {
		return 0
	}
	rows, err := f.unpriced.FetchUnpricedBatch(ctx, fetchBatchLimit)
	if err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing unpriced recovery fetch failed", logx.Error(err))
		}
		return 0
	}
	if len(rows) == 0 {
		return 0
	}
	repriced := make([]domain.RepricedUsage, 0, len(rows))
	for _, row := range rows {
		multiplier := 10_000
		if f.bal != nil {
			multiplier = f.bal.EffectiveMultiplier(row.UserID, row.GroupID)
		}
		if result, ok := RepriceUsage(row, f.resolver, multiplier); ok {
			repriced = append(repriced, result)
		}
	}
	if len(repriced) == 0 {
		return 0
	}
	updated, err := f.unpriced.ApplyRepricedBatch(ctx, repriced)
	if err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing unpriced recovery apply failed", logx.Error(err))
		}
		return 0
	}
	return updated
}
