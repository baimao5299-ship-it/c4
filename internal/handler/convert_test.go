// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckedNumericConversionsRejectNonFiniteAndOverflow(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := usdToMillisChecked(value)
		require.Error(t, err)
		_, err = normalToMultChecked(value)
		require.Error(t, err)
	}
	// Scaling this finite value reaches the signed 2^63 boundary.
	_, err := usdToMillisChecked(math.Ldexp(1, 63))
	require.Error(t, err)
	_, err = usdToMillisChecked(math.Ldexp(1, 63) / currencyScale)
	require.Error(t, err)
}

func TestAPIConversionBoundariesRejectNonFiniteValues(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := apiMultiplierToMillis(&value)
		require.Error(t, err)
		_, err = apiRedemptionValueToMillis("balance", value)
		require.Error(t, err)
	}
	bad := math.NaN()
	_, err := usdToMillisPtr(&bad)
	require.Error(t, err)
	_, err = usdPerCallToMilliPtr(&bad)
	require.Error(t, err)
	_, err = usdPerImageToMilliPtr(&bad)
	require.Error(t, err)
}

func TestPriceConversionsRejectSilentRounding(t *testing.T) {
	// Prices are stored in 1e-5 USD units. Values on that grid, including the
	// small positive value that previously looked free, must round-trip exactly.
	for _, value := range []float64{0, 0.00001, 0.001, 0.08, 1.25} {
		got, err := usdToMillisPtr(&value)
		require.NoError(t, err, "value=%v", value)
		require.NotNil(t, got)
		require.InDelta(t, value, float64(*got)/currencyScale, 1e-12)
	}
	for _, value := range []float64{0.000011, 0.000019, 0.001001} {
		_, err := usdToMillisPtr(&value)
		require.Error(t, err, "value=%v must not be silently rounded", value)
	}
	// A negative value that rounds to zero must still be rejected. Otherwise it
	// would pass the service's integer non-negative check as an accidental free
	// price.
	for _, value := range []float64{-0.000001, -0.000004, -0.000009} {
		for name, convert := range map[string]func(*float64) (*int64, error){
			"token": usdToMillisPtr,
			"image": usdPerImageToMilliPtr,
			"call":  usdPerCallToMilliPtr,
		} {
			_, err := convert(&value)
			require.Error(t, err, "%s value=%v must remain negative", name, value)
		}
	}
}

func TestMultiplierPrecisionPreservesSmallPositiveValues(t *testing.T) {
	for _, tc := range []struct {
		value float64
		want  int
	}{
		{value: 0, want: 0},
		{value: 0.001, want: 10},
		{value: 0.08, want: 800},
		{value: 1, want: 10000},
	} {
		got, err := apiMultiplierToMillis(&tc.value)
		require.NoError(t, err, "value=%v", tc.value)
		require.NotNil(t, got)
		require.Equal(t, tc.want, *got, "value=%v", tc.value)
	}
	for _, tooSmall := range []float64{0.00001, 1e-13} {
		_, err := apiMultiplierToMillis(&tooSmall)
		require.Error(t, err, "positive value %g below storage precision must not become free", tooSmall)
	}
	inexact := 0.00015
	_, err := apiMultiplierToMillis(&inexact)
	require.Error(t, err, "values must not be silently rounded to a different multiplier")
}

func TestMultiplierMapRejectsValuesThatWouldBecomeFree(t *testing.T) {
	value := 0.00001
	_, err := apiMultiplierMap(map[string]*float64{"7": &value})
	require.Error(t, err)

	value = 0.001
	got, err := apiMultiplierMap(map[string]*float64{"7": &value})
	require.NoError(t, err)
	require.NotNil(t, got[7])
	require.Equal(t, 10, *got[7])

	zero := 0.0
	got, err = apiMultiplierMap(map[string]*float64{"7": &zero})
	require.NoError(t, err)
	require.NotNil(t, got[7])
	require.Equal(t, 0, *got[7])
}
