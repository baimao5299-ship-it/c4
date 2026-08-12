// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDefaultSettingPricingKeys litellm 价格同步 2 key 内置注册表默认值命中。
func TestDefaultSettingPricingKeys(t *testing.T) {
	src := DefaultSetting("price_source_url")
	require.NotNil(t, src, "price_source_url 内置注册")
	require.Equal(t, SettingTypeString, src.Type)
	require.Equal(t, "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json", src.Value)

	cron := DefaultSetting("price_sync_cron")
	require.NotNil(t, cron, "price_sync_cron 内置注册")
	require.Equal(t, SettingTypeString, cron.Type)
	require.Equal(t, "0 3 * * *", cron.Value)
}

// TestPricingSourceValid PricingSource 合法性校验。
func TestPricingSourceValid(t *testing.T) {
	for _, s := range []PricingSource{PricingSourceLitellm, PricingSourceManual} {
		require.True(t, s.Valid(), "source %s 合法", s)
	}
	require.False(t, PricingSource("scheduled").Valid())
}
