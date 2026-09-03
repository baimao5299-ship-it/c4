// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestCoalescePricedModelRows(t *testing.T) {
	price := func(value int64) *int64 { return &value }
	models := func(rows []PublicChannelModelPrice) []string {
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.Model)
		}
		return out
	}

	cases := []struct {
		name    string
		rows    []PublicChannelModelPrice
		want    []string
		wantIn  *int64
		checkIn bool
	}{
		{name: "empty input", want: []string{}},
		{name: "single priced row", rows: []PublicChannelModelPrice{{Model: "a", InputPerM: price(1)}}, want: []string{"a"}},
		{name: "single unpriced row", rows: []PublicChannelModelPrice{{Model: "a"}}, want: []string{"a"}},
		{name: "same name same price coalesces", rows: []PublicChannelModelPrice{{Model: "A.b", InputPerM: price(1)}, {Model: "a-b", InputPerM: price(1)}}, want: []string{"A.b"}},
		{name: "same name different price remains", rows: []PublicChannelModelPrice{{Model: "A.b", InputPerM: price(1)}, {Model: "a-b", InputPerM: price(2)}}, want: []string{"A.b", "a-b"}},
		{name: "same name unpriced first priced second", rows: []PublicChannelModelPrice{{Model: "A.b"}, {Model: "a-b", InputPerM: price(1)}}, want: []string{"A.b"}, wantIn: price(1), checkIn: true},
		{name: "same name priced first unpriced second", rows: []PublicChannelModelPrice{{Model: "A.b", InputPerM: price(1)}, {Model: "a-b"}}, want: []string{"A.b"}, wantIn: price(1), checkIn: true},
		{name: "same name all unpriced", rows: []PublicChannelModelPrice{{Model: "A.b"}, {Model: "a-b"}}, want: []string{"A.b"}},
		{name: "three spellings two prices", rows: []PublicChannelModelPrice{{Model: "a.b", InputPerM: price(1)}, {Model: "a-b", InputPerM: price(1)}, {Model: "a_b", InputPerM: price(2)}}, want: []string{"a.b", "a_b"}},
		{name: "three spellings first uniquely priced", rows: []PublicChannelModelPrice{{Model: "a.b", InputPerM: price(1)}, {Model: "a-b", InputPerM: price(2)}, {Model: "a_b", InputPerM: price(2)}}, want: []string{"a.b", "a-b", "a_b"}},
		{name: "different modes remain", rows: []PublicChannelModelPrice{{Model: "a.b", Mode: domain.PriceModeToken, InputPerM: price(1)}, {Model: "a-b", Mode: domain.PriceModeCall, InputPerM: price(1)}}, want: []string{"a.b", "a-b"}},
		{name: "different official prices remain", rows: []PublicChannelModelPrice{{Model: "a.b", OfficialInputPerM: price(1)}, {Model: "a-b", OfficialInputPerM: price(2)}}, want: []string{"a.b", "a-b"}},
		{name: "different models do not interact", rows: []PublicChannelModelPrice{{Model: "a", InputPerM: price(1)}, {Model: "b", InputPerM: price(2)}}, want: []string{"a", "b"}},
		{name: "empty model remains", rows: []PublicChannelModelPrice{{Model: ""}, {Model: "a", InputPerM: price(1)}}, want: []string{"", "a"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := coalescePricedModelRows(tc.rows)
			require.Equal(t, tc.want, models(got))
			if tc.checkIn {
				require.Len(t, got, 1)
				require.Equal(t, tc.wantIn, got[0].InputPerM)
			}
		})
	}
}
