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
		in, want string
	}{
		{"", "no_price:m800"},
		{"priority", "no_price:priority:m12500"},
		{"turbo", "no_price"},
	} {
		log := &domain.UsageLog{BillingTier: tc.in}
		markNoPrice(log, map[string]int{"": 800, "priority": 12500, "turbo": 10000}[tc.in])
		if log.BillingTier != tc.want {
			t.Fatalf("marker = %q, want %q", log.BillingTier, tc.want)
		}
	}
}
