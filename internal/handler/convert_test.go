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
