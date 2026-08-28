// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupRoutingModeValidValues(t *testing.T) {
	require.True(t, GroupRoutingModeAccounts.Valid())
	require.True(t, GroupRoutingModeUpstreams.Valid())
	require.False(t, GroupRoutingMode("").Valid())
	require.False(t, GroupRoutingMode("mixed").Valid())
}

func TestGroupEffectiveRoutingModeKeepsLegacyGroupsOnAccounts(t *testing.T) {
	var nilGroup *Group
	require.Equal(t, GroupRoutingModeAccounts, nilGroup.EffectiveRoutingMode())
	require.Equal(t, GroupRoutingModeAccounts, (&Group{}).EffectiveRoutingMode())
	require.Equal(t, GroupRoutingModeUpstreams, (&Group{RoutingMode: GroupRoutingModeUpstreams}).EffectiveRoutingMode())
}

func TestGroupUpstreamCarriesIndependentMembershipPolicy(t *testing.T) {
	m := &GroupUpstream{
		GroupID: 11, UpstreamID: 22, Weight: 30, Priority: 2,
		MaxConcurrency: 4, Enabled: true,
	}
	require.Equal(t, int64(11), m.GroupID)
	require.Equal(t, int64(22), m.UpstreamID)
	require.Equal(t, 30, m.Weight)
	require.Equal(t, 2, m.Priority)
	require.Equal(t, 4, m.MaxConcurrency)
	require.True(t, m.Enabled)
}
