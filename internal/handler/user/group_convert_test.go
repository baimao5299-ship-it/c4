// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestUserGroupResponseIncludesRoutingPolicy(t *testing.T) {
	got := toAPIGroup(&domain.Group{
		ID: 7, Name: "relay", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-5", "gpt-5-codex"},
	})

	require.NotNil(t, got.RoutingMode)
	require.Equal(t, Upstreams, *got.RoutingMode)
	require.NotNil(t, got.AllowedModels)
	require.Equal(t, []string{"gpt-5", "gpt-5-codex"}, *got.AllowedModels)
}

func TestUserGroupResponseNormalizesLegacyRoutingPolicy(t *testing.T) {
	got := toAPIGroup(&domain.Group{ID: 8, Name: "accounts", Visibility: domain.GroupVisibilityPublic})

	require.NotNil(t, got.RoutingMode)
	require.Equal(t, Accounts, *got.RoutingMode)
	require.NotNil(t, got.AllowedModels)
	require.Empty(t, *got.AllowedModels)
}
