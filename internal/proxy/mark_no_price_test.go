// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestMarkNoPriceNormalizesAndPreservesTier(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty defaults", in: "", want: "no_price"},
		{name: "auto defaults", in: "auto", want: "no_price"},
		{name: "plain marker stays plain", in: "no_price", want: "no_price"},
		{name: "existing marker is idempotent", in: "no_price:priority", want: "no_price:priority"},
		{name: "priority preserved", in: "priority", want: "no_price:priority"},
		{name: "fast preserved", in: "fast", want: "no_price:fast"},
		{name: "flex preserved", in: "flex", want: "no_price:flex"},
		{name: "unknown tier is plain marker", in: "turbo", want: "no_price"},
		{name: "whitespace and case normalized", in: "  PrIoRiTy  ", want: "no_price:priority"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &domain.UsageLog{BillingTier: tt.in}
			markNoPrice(log)
			require.Equal(t, tt.want, log.BillingTier)
		})
	}
}

func TestMarkNoPriceNilSafe(t *testing.T) {
	markNoPrice(nil)
}

func TestMarkNoPriceCapturesRequestMultiplier(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		mult     int
		want     string
	}{
		{name: "discounted group", in: "", mult: 800, want: "no_price:m800"},
		{name: "premium tier and multiplier", in: "priority", mult: 12500, want: "no_price:priority:m12500"},
		// The default multiplier is recorded like any other. It used to be
		// omitted as redundant, but an absent marker makes recoveryMultiplier
		// fall back to the multiplier in force when the backfill runs, so an
		// administrative rate change between request and backfill repriced the
		// row at the new rate. Unknown tiers still collapse to the plain marker.
		{name: "default multiplier is still recorded", in: "turbo", mult: 10000, want: "no_price:m10000"},
		{name: "free group", in: "", mult: 0, want: "no_price:m0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &domain.UsageLog{BillingTier: tc.in}
			markNoPrice(log, tc.mult)
			if log.BillingTier != tc.want {
				t.Fatalf("marker = %q, want %q", log.BillingTier, tc.want)
			}
		})
	}
}
