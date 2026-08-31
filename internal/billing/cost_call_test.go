// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func int64Ptr(v int64) *int64 { return &v }

func TestCallCostFromResolved_ZeroPrice_NoPanic(t *testing.T) {
	rp := domain.ResolvedPrices{PricePerCall: int64Ptr(0)}
	require.NotPanics(t, func() {
		require.Equal(t, int64(0), CallCostFromResolved(rp, 10))
		require.Equal(t, int64(0), CallCostFromResolved(rp, 0))
		require.Equal(t, int64(0), CallCostFromResolved(rp, 1<<60))
	})
	// nil price also 0
	require.Equal(t, int64(0), CallCostFromResolved(domain.ResolvedPrices{PricePerCall: nil}, 10))
}

func TestCallCostFromResolved_NegativePrice_Defense(t *testing.T) {
	rp := domain.ResolvedPrices{PricePerCall: int64Ptr(-5)}
	require.Equal(t, int64(0), CallCostFromResolved(rp, 10))
	rp2 := domain.ResolvedPrices{PricePerCall: int64Ptr(-1 << 60)}
	require.Equal(t, int64(0), CallCostFromResolved(rp2, 1<<30))
}

func TestImageCostFromResolved_ZeroPrice_NoPanic(t *testing.T) {
	rp := domain.ResolvedPrices{PricePerImage: int64Ptr(0), ImgInTokPerM: int64Ptr(0), ImgOutTokPerM: int64Ptr(0)}
	require.Equal(t, int64(0), ImageCostFromResolved(rp, 100, 100, 10))
}

func TestCostFromResolved_ZeroPrice(t *testing.T) {
	zero := int64(0)
	rp := domain.ResolvedPrices{InputPerM: &zero, OutputPerM: &zero, CacheReadPerM: &zero, CacheWritePerM: &zero}
	require.Equal(t, int64(0), CostFromResolved(rp, 100, 200, 300, 400))
}

func TestCostFromResolved_NegativePricesAreIgnored(t *testing.T) {
	neg := int64(-10)
	positive := int64(20)
	rp := domain.ResolvedPrices{InputPerM: &neg, OutputPerM: &positive, CacheReadPerM: &neg, CacheWritePerM: &neg}
	// Only the positive output price contributes; a corrupted negative price
	// must never turn usage into a credit or wrap the settlement arithmetic.
	require.Equal(t, int64(2), CostFromResolved(rp, 0, 100000, 100000, 100000))
}

func TestImageCostFromResolved_NegativePricesAreIgnored(t *testing.T) {
	neg := int64(-10)
	perImage := int64(100)
	rp := domain.ResolvedPrices{ImgInTokPerM: &neg, ImgOutTokPerM: &neg, PricePerImage: &perImage}
	require.Equal(t, int64(200), ImageCostFromResolved(rp, 1000000, 1000000, 2))
}

func TestCostArithmeticSaturatesExtremeComponents(t *testing.T) {
	max := int64(math.MaxInt64)
	price := int64(1_000_000_000)
	rp := domain.ResolvedPrices{
		InputPerM:      &price,
		OutputPerM:     &price,
		CacheReadPerM:  &price,
		CacheWritePerM: &price,
	}
	// The exact cap is an implementation detail; the important contract is a
	// positive, bounded debit rather than an int64 wraparound into a credit.
	require.Greater(t, CostFromResolved(rp, max, max, max, max), int64(0))
	require.LessOrEqual(t, CostFromResolved(rp, max, max, max, max), max)

	imagePrice := price
	img := domain.ResolvedPrices{
		ImgInTokPerM:  &max,
		ImgOutTokPerM: &max,
		PricePerImage: &imagePrice,
	}
	got := ImageCostFromResolved(img, max, max, max)
	require.Greater(t, got, int64(0))
	require.LessOrEqual(t, got, max)
}

func TestCostFromResolved_HighTokenPricesNeverBecomeFree(t *testing.T) {
	high := int64(math.MaxInt64)
	for name, rp := range map[string]domain.ResolvedPrices{
		"input":       {InputPerM: &high},
		"output":      {OutputPerM: &high},
		"cache_read":  {CacheReadPerM: &high},
		"cache_write": {CacheWritePerM: &high},
	} {
		t.Run(name, func(t *testing.T) {
			got := CostFromResolved(rp, 1, 1, 1, 1)
			require.Greater(t, got, int64(0), "a positive token price must never be rounded to a free request")
			require.LessOrEqual(t, got, int64(math.MaxInt64))
		})
	}
}

func TestImageCostFromResolved_HighPricesNeverBecomeFree(t *testing.T) {
	high := int64(math.MaxInt64)
	for name, rp := range map[string]domain.ResolvedPrices{
		"input_tokens":  {ImgInTokPerM: &high},
		"output_tokens": {ImgOutTokPerM: &high},
		"per_image":     {PricePerImage: &high},
	} {
		t.Run(name, func(t *testing.T) {
			got := ImageCostFromResolved(rp, 1, 1, 1)
			require.Greater(t, got, int64(0), "a positive image price must never be rounded to a free request")
			require.LessOrEqual(t, got, int64(math.MaxInt64))
		})
	}
}

func TestCallCostFromResolved_HighPriceSaturatesInsteadOfBecomingFree(t *testing.T) {
	high := int64(math.MaxInt64)
	got := CallCostFromResolved(domain.ResolvedPrices{PricePerCall: &high}, 1)
	require.Equal(t, int64(imageTotalCap), got)

	// A count larger than the cap is also bounded without overflowing or
	// turning the charge into zero.
	require.Equal(t, int64(imageTotalCap), CallCostFromResolved(
		domain.ResolvedPrices{PricePerCall: &high}, math.MaxInt64,
	))
}
