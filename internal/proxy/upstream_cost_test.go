// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package proxy

import (
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func upstreamCostLog(upstreamID int64, multiplier int, rawCost, userCost int64, price int64) *domain.UsageLog {
	return &domain.UsageLog{
		UpstreamID:           upstreamID,
		UpstreamMultiplierBP: &multiplier,
		RawCost:              rawCost,
		Cost:                 userCost,
		Model:                "model",
		PriceInputMillis:     &price,
		Format:               domain.FormatOpenAIChat,
	}
}

func TestApplyMultiplierSupportsValidatedUpstream100x(t *testing.T) {
	// Upstream management accepts up to 1,000,000 basis points (100x). The
	// economics path must not silently reduce that to the old 10x cap.
	require.Equal(t, int64(200), applyUpstreamMultiplier(10, 200_000))
	require.Equal(t, int64(1_000), applyUpstreamMultiplier(10, 1_000_000))
}

func TestUpstreamEconomicsCarriesLowMultiplierAcrossRequests(t *testing.T) {
	var p Proxy
	const requests = 100
	var total int64
	for i := 0; i < requests; i++ {
		log := upstreamCostLog(17, 800, 1, 1, 1) // x0.08 per raw ledger unit
		p.applyUpstreamEconomics(log)
		require.NotNil(t, log.UpstreamCost)
		total += *log.UpstreamCost
	}
	// 100 * 1 * 0.08 = 8. Rounding each row independently would emit zero
	// for all requests and understate the upstream total completely.
	require.Equal(t, int64(8), total)
}

func TestUpstreamEconomicsUses100xCeiling(t *testing.T) {
	var p Proxy
	log := upstreamCostLog(19, 200_000, 10_000, 250_000, 1)
	p.applyUpstreamEconomics(log)
	require.Equal(t, int64(200_000), *log.UpstreamCost)
}

func TestUpstreamEconomicsCarryIsolatedByUpstreamAndPriceSnapshot(t *testing.T) {
	var p Proxy
	// Leave an 0.08 fraction on upstream 17.
	first := upstreamCostLog(17, 800, 1, 1, 100)
	p.applyUpstreamEconomics(first)
	require.Zero(t, *first.UpstreamCost)

	// A different upstream cannot consume that fraction.
	other := upstreamCostLog(18, 800, 1, 1, 100)
	p.applyUpstreamEconomics(other)
	require.Zero(t, *other.UpstreamCost)

	// A changed price snapshot for the original upstream starts a new bucket.
	changed := upstreamCostLog(17, 800, 1, 1, 200)
	p.applyUpstreamEconomics(changed)
	require.Zero(t, *changed.UpstreamCost)

	// Returning to the original snapshot settles its own second fraction.
	original := upstreamCostLog(17, 800, 12, 12, 100)
	p.applyUpstreamEconomics(original)
	require.Equal(t, int64(1), *original.UpstreamCost)
}
