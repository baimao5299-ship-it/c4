// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

func TestApplySelectionAttributionRecordsLegacyAccountEndpoint(t *testing.T) {
	log := &domain.UsageLog{}
	sel := &scheduler.Selection{
		TargetKind: scheduler.TargetKindAccount,
		BaseURL:    "https://legacy-user:secret@relay.example.test:9443/v1?tenant=private#fragment",
	}

	applySelectionAttribution(log, sel)

	require.Equal(t, "account", log.TargetKind)
	require.Equal(t, "relay.example.test:9443", log.UpstreamHost)
	require.Zero(t, log.UpstreamID)
	require.Empty(t, log.UpstreamName)
	require.Nil(t, log.UpstreamMultiplierBP)
}

func TestApplySelectionAttributionSanitizesManagedHostAndFallsBackToBaseURL(t *testing.T) {
	tests := []struct {
		name      string
		selection *scheduler.Selection
		wantHost  string
		wantID    int64
	}{
		{
			name: "authority snapshot",
			selection: &scheduler.Selection{
				TargetKind:   scheduler.TargetKindUpstreamMember,
				UpstreamID:   7,
				UpstreamName: "primary",
				UpstreamHost: "api.example.test:8443",
				BaseURL:      "https://fallback.example.test/v1",
			},
			wantHost: "api.example.test:8443",
			wantID:   7,
		},
		{
			name: "base URL fallback",
			selection: &scheduler.Selection{
				TargetKind: scheduler.TargetKindUpstreamMember,
				UpstreamID: 9,
				BaseURL:    "https://fallback-user:secret@fallback.example.test/v1",
			},
			wantHost: "fallback.example.test",
			wantID:   9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &domain.UsageLog{}
			applySelectionAttribution(log, tt.selection)

			require.Equal(t, tt.wantHost, log.UpstreamHost)
			require.Equal(t, tt.wantID, log.UpstreamID)
			require.NotNil(t, log.UpstreamMultiplierBP)
		})
	}
}

func TestAttributionHostRejectsInvalidEndpoint(t *testing.T) {
	for _, raw := range []string{"", "relative path", "https://", "https://[bad"} {
		require.Empty(t, attributionHost(raw), raw)
	}
}
