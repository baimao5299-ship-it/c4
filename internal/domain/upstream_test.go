// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamStabilityHasExplicitUnknownState(t *testing.T) {
	score, rating, latency := (*Upstream)(nil).Stability()
	require.Equal(t, 0, score)
	require.Equal(t, "unknown", rating)
	require.Equal(t, int64(0), latency)

	score, rating, latency = (&Upstream{}).Stability()
	require.Equal(t, 0, score)
	require.Equal(t, "unknown", rating)
	require.Equal(t, int64(0), latency)
}

func TestUpstreamStabilityRatingBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		requests    int64
		successes   int64
		latencySum  int64
		wantScore   int
		wantRating  string
		wantLatency int64
	}{
		{name: "excellent boundary", requests: 100, successes: 99, latencySum: 80_000, wantScore: 99, wantRating: "excellent", wantLatency: 800},
		{name: "good boundary", requests: 100, successes: 97, latencySum: 150_000, wantScore: 97, wantRating: "good", wantLatency: 1_500},
		{name: "fair boundary", requests: 100, successes: 90, latencySum: 300_000, wantScore: 90, wantRating: "fair", wantLatency: 3_000},
		{name: "poor below threshold", requests: 100, successes: 89, latencySum: 300_000, wantScore: 89, wantRating: "poor", wantLatency: 3_000},
		{name: "slow excellent score is good", requests: 100, successes: 100, latencySum: 80_100, wantScore: 100, wantRating: "good", wantLatency: 801},
		{name: "too slow for every tier", requests: 100, successes: 100, latencySum: 300_100, wantScore: 100, wantRating: "poor", wantLatency: 3_001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &Upstream{RequestCount: tc.requests, SuccessCount: tc.successes, LatencyTotalMS: tc.latencySum}
			score, rating, latency := u.Stability()
			require.Equal(t, tc.wantScore, score)
			require.Equal(t, tc.wantRating, rating)
			require.Equal(t, tc.wantLatency, latency)
		})
	}
}

func TestUpstreamStabilityClampsCorruptCounters(t *testing.T) {
	u := &Upstream{RequestCount: 10, SuccessCount: 99, LatencyTotalMS: -10}
	score, rating, latency := u.Stability()
	require.Equal(t, 100, score)
	require.Equal(t, "excellent", rating)
	require.Equal(t, int64(0), latency)
}
