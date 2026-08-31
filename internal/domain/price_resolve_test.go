// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func int64PtrD(v int64) *int64 { return &v }
func intPtrD(v int) *int       { return &v }

func TestResolveEntryPrices_SetPricePerCall(t *testing.T) {
	entry := &PriceEntry{Model: "search-m", Mode: PriceModeCall, PricePerCall: int64PtrD(1000)}
	custom := int64(9999)
	variants := []*PriceVariant{{Model: "search-m", Seq: 1, SetPricePerCall: &custom}}
	rp, ok := ResolveEntryPrices(entry, variants, "auto", 0, time.Now())
	require.True(t, ok)
	require.NotNil(t, rp.PricePerCall)
	require.Equal(t, custom, *rp.PricePerCall)
}

func TestResolveEntryPrices_ImageOverrides(t *testing.T) {
	entry := &PriceEntry{Model: "img-m", Mode: PriceModeImage, ImgInTokPerM: int64PtrD(100), ImgOutTokPerM: int64PtrD(200), PricePerImage: int64PtrD(300)}
	imgIn := int64(111)
	imgOut := int64(222)
	perImg := int64(333)
	variants := []*PriceVariant{{Model: "img-m", Seq: 1, SetImgInTokPerM: &imgIn, SetImgOutTokPerM: &imgOut, SetPricePerImage: &perImg}}
	rp, ok := ResolveEntryPrices(entry, variants, "auto", 0, time.Now())
	require.True(t, ok)
	require.Equal(t, imgIn, *rp.ImgInTokPerM)
	require.Equal(t, imgOut, *rp.ImgOutTokPerM)
	require.Equal(t, perImg, *rp.PricePerImage)
}

func TestResolveEntryPrices_CacheOverrides(t *testing.T) {
	entry := &PriceEntry{Model: "tok-m", Mode: PriceModeToken, InputPerM: int64PtrD(1000), OutputPerM: int64PtrD(2000), CacheReadPerM: int64PtrD(3000), CacheWritePerM: int64PtrD(4000)}
	cr := int64(999)
	cw := int64(888)
	variants := []*PriceVariant{{Model: "tok-m", Seq: 1, SetCacheReadPerM: &cr, SetCacheCreationPerM: &cw}}
	rp, ok := ResolveEntryPrices(entry, variants, "auto", 0, time.Now())
	require.True(t, ok)
	require.Equal(t, cr, *rp.CacheReadPerM)
	require.Equal(t, cw, *rp.CacheWritePerM)
	// input/output unchanged
	require.Equal(t, int64(1000), *rp.InputPerM)
}

func TestResolveEntryPrices_MultBPValidation(t *testing.T) {
	base := int64(1000000)
	entry := &PriceEntry{Model: "m", Mode: PriceModeToken, InputPerM: int64PtrD(base), OutputPerM: int64PtrD(base)}
	// large valid price with mult 100000 should stay non-negative (no overflow wrap)
	large := int64(1_000_000_000)
	entry2 := &PriceEntry{Model: "m2", Mode: PriceModeToken, InputPerM: int64PtrD(large), OutputPerM: int64PtrD(large)}
	variants := []*PriceVariant{{Model: "m2", Seq: 1, MultBP: intPtrD(100000)}}
	rp, ok := ResolveEntryPrices(entry2, variants, "auto", 0, time.Now())
	require.True(t, ok)
	require.NotNil(t, rp.InputPerM)
	require.GreaterOrEqual(t, *rp.InputPerM, int64(0))
	// Invalid persisted multipliers fail closed instead of changing the price.
	variantsNeg := []*PriceVariant{{Model: "m", Seq: 1, MultBP: intPtrD(-5000)}}
	rp, ok = ResolveEntryPrices(entry, variantsNeg, "auto", 0, time.Now())
	require.False(t, ok)
	// Values above the supported x10 maximum fail closed as well.
	variantsHuge := []*PriceVariant{{Model: "m", Seq: 1, MultBP: intPtrD(999999)}}
	rp, ok = ResolveEntryPrices(entry, variantsHuge, "auto", 0, time.Now())
	require.False(t, ok)
	// mult 5000 means ×0.5 exactly: 1_000_000*5000/10000=500000
	variantsHalf := []*PriceVariant{{Model: "m", Seq: 1, MultBP: intPtrD(5000)}}
	rp, ok = ResolveEntryPrices(entry, variantsHalf, "auto", 0, time.Now())
	require.True(t, ok)
	require.Equal(t, int64(500000), *rp.InputPerM)
}

func TestResolveEntryPrices_MultBPOverflowFailsClosed(t *testing.T) {
	entry := &PriceEntry{Model: "huge", Mode: PriceModeToken, InputPerM: int64PtrD(maxPriceInt64)}
	rp, ok := ResolveEntryPrices(entry, []*PriceVariant{{Model: "huge", Seq: 1, MultBP: intPtrD(100000)}}, "auto", 0, time.Now())
	require.False(t, ok, "an overflowing conditional price must not be silently saturated")

	// A nil variant can occur in a partially built test/config snapshot and is
	// skipped rather than dereferenced.
	rp, ok = ResolveEntryPrices(entry, []*PriceVariant{nil}, "auto", 0, time.Now())
	require.True(t, ok)
	require.Equal(t, maxPriceInt64, *rp.InputPerM)
}

func TestResolveEntryPrices_MultBPMustStayOnPriceGrid(t *testing.T) {
	for _, price := range []int64{100, 500} {
		entry := &PriceEntry{Model: "tiny", Mode: PriceModeToken, InputPerM: int64PtrD(price)}
		_, ok := ResolveEntryPrices(entry, []*PriceVariant{{Model: "tiny", Seq: 1, MultBP: intPtrD(10)}}, "auto", 0, time.Now())
		require.False(t, ok, "price=%d multiplied by 0.001 cannot be represented exactly", price)
	}

	entry := &PriceEntry{Model: "exact", Mode: PriceModeToken, InputPerM: int64PtrD(10_000)}
	rp, ok := ResolveEntryPrices(entry, []*PriceVariant{{Model: "exact", Seq: 1, MultBP: intPtrD(10)}}, "auto", 0, time.Now())
	require.True(t, ok)
	require.Equal(t, int64(10), *rp.InputPerM)
}

func TestResolveEntryPrices_OverrideBypassesInexactBaseProduct(t *testing.T) {
	base, override, mult := int64(100), int64(7), 10
	entry := &PriceEntry{Model: "override", Mode: PriceModeToken, InputPerM: &base}
	variant := &PriceVariant{Model: "override", Seq: 1, MultBP: &mult, SetInputPerM: &override}
	rp, ok := ResolveEntryPrices(entry, []*PriceVariant{variant}, "auto", 0, time.Now())
	require.True(t, ok)
	require.Equal(t, override, *rp.InputPerM)
	require.NoError(t, ValidateVariantPricePrecision(entry, variant))
}
