// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScaledPricePreservesGroupMultiplier(t *testing.T) {
	base := int64(100000) // $1.00 per million tokens in the pricing store

	got := scaledPrice(&base, 0.08)
	require.NotNil(t, got)
	require.InDelta(t, 0.08, *got, 1e-12, "x0.08 must not fall back to x1")

	got = scaledPrice(&base, 0)
	require.NotNil(t, got)
	require.Equal(t, 0.0, *got, "an explicit free group remains free")
	require.Nil(t, scaledPrice(nil, 0.08), "missing catalogue price stays null")
}
