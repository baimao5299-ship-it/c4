// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestUpdateSettingNumberRange A-P2-11 值域护栏：注册表 Min/Max 是单一事实源——
// 低于 Min → 400（含负值直达新注册用户的攻击形态 default_user_balance=-500）；
// Min 恰好命中（边界值）→ 接受；合法正数 → 接受。消费端零改动（护栏前置）。
func TestUpdateSettingNumberRange(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		key   string
		below string // 低于 Min → 400
		atMin string // 恰好命中 Min → 接受
	}{
		{"default_user_max_concurrency", "default_user_max_concurrency", "-1", "0"},
		{"default_user_balance", "default_user_balance", "-500", "0"},
		{"default_user_temp_balance", "default_user_temp_balance", "-1", "0"},
		{"default_user_temp_balance_ttl_days", "default_user_temp_balance_ttl_days", "-1", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
			_, err := svc.UpdateSetting(ctx, tc.key, tc.below)
			require.ErrorIs(t, err, ErrInvalidInput, "低于 Min → 400: %s=%s", tc.key, tc.below)
			_, err = svc.UpdateSetting(ctx, tc.key, tc.atMin)
			require.NoError(t, err, "Min 边界命中 → 接受: %s=%s", tc.key, tc.atMin)
			_, err = svc.UpdateSetting(ctx, tc.key, "12345")
			require.NoError(t, err, "合法正数 → 接受: %s", tc.key)
		})
	}
}

// TestUpdateSettingNumberNoMax 无 Max 条目（注册表未声明上界）：大数不受上界
// 护栏限制（仅下界生效）。
func TestUpdateSettingNumberNoMax(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	_, err := svc.UpdateSetting(ctx, "default_user_max_concurrency", "1000000000000")
	require.NoError(t, err, "Max nil（无上界）→ 不越界")
}

func TestCodexTLSConvergenceSettingAppliesOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, log: nil}
	svc.reloadSettings(ctx)

	var applied []bool
	require.NoError(t, svc.SetCodexTLSConvergenceApply(func(enabled bool) error {
		applied = append(applied, enabled)
		return nil
	}))
	require.Equal(t, []bool{false}, applied, "default state is applied when the transport hook is installed")

	_, err := svc.UpdateSetting(ctx, "codex_tls_convergence_enabled", "true")
	require.NoError(t, err)
	require.Equal(t, []bool{false, true}, applied)

	require.NoError(t, svc.ReloadSettings(ctx))
	require.Equal(t, []bool{false, true}, applied, "unrelated settings reloads do not rebuild the transport")
}

func TestCodexTLSConvergenceUsesStartupDefaultUntilExplicitOverride(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, log: nil}
	svc.reloadSettings(ctx)
	svc.SetInitialCodexTLSConvergence(true)
	var applied []bool
	require.NoError(t, svc.SetCodexTLSConvergenceApply(func(enabled bool) error {
		applied = append(applied, enabled)
		return nil
	}))
	require.Equal(t, []bool{true}, applied)

	_, err := svc.UpdateSetting(ctx, "codex_tls_convergence_enabled", "false")
	require.NoError(t, err)
	require.Equal(t, []bool{true, false}, applied)
}

func TestCodexTLSConvergenceHonorsPersistedFalseOverStartupDefault(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	_, err := fs.SetSetting(ctx, "codex_tls_convergence_enabled", domain.SettingTypeSwitch, "false")
	require.NoError(t, err)
	// The real repository stamps persisted rows. The fake store keeps the
	// explicit marker here so this test exercises the production distinction.
	fs.settings["codex_tls_convergence_enabled"].UpdatedAt = time.Now()
	svc := &Service{store: fs, log: nil}
	svc.reloadSettings(ctx)
	svc.SetInitialCodexTLSConvergence(true)
	var applied []bool
	require.NoError(t, svc.SetCodexTLSConvergenceApply(func(enabled bool) error {
		applied = append(applied, enabled)
		return nil
	}))
	require.Equal(t, []bool{false}, applied)
}

func TestCodexTLSConvergenceSettingRollsBackWhenRuntimeRejects(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, log: nil}
	svc.reloadSettings(ctx)
	require.NoError(t, svc.SetCodexTLSConvergenceApply(func(enabled bool) error {
		if enabled {
			return context.DeadlineExceeded
		}
		return nil
	}))

	_, err := svc.UpdateSetting(ctx, "codex_tls_convergence_enabled", "true")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	saved, err := fs.GetSetting(ctx, "codex_tls_convergence_enabled")
	require.NoError(t, err)
	require.Equal(t, "false", saved.Value)
	require.Equal(t, "false", svc.settingValue("codex_tls_convergence_enabled"))
}

func TestUpstreamProxySettingUsesStartupFallbackAndLiveOverride(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, log: nil}
	svc.SetInitialUpstreamProxyURL("http://fixture:7892")
	var applied []string
	svc.SetUpstreamProxyApply(func(_ context.Context, raw string) error {
		applied = append(applied, raw)
		return nil
	})
	svc.reloadSettings(ctx)
	require.Equal(t, []string{"http://fixture:7892"}, applied, "inherit uses startup route")

	_, err := svc.UpdateSetting(ctx, "upstream_proxy_url", "http://fixture:7897")
	require.NoError(t, err)
	require.Equal(t, "http://fixture:7897", svc.UpstreamProxyURL())
	require.Equal(t, []string{"http://fixture:7892", "http://fixture:7897"}, applied)

	_, err = svc.UpdateSetting(ctx, "upstream_proxy_url", "direct")
	require.NoError(t, err)
	require.Empty(t, svc.UpstreamProxyURL())
	require.Equal(t, []string{"http://fixture:7892", "http://fixture:7897", ""}, applied)
}

func TestUpstreamProxySettingRollsBackWhenTransportRejects(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, log: nil}
	svc.SetInitialUpstreamProxyURL("http://fixture:7892")
	svc.SetUpstreamProxyApply(func(_ context.Context, raw string) error {
		if raw == "http://fixture:bad" {
			return context.DeadlineExceeded
		}
		return nil
	})
	svc.reloadSettings(ctx)
	_, err := svc.UpdateSetting(ctx, "upstream_proxy_url", "http://fixture:bad")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, "http://fixture:7892", svc.UpstreamProxyURL(), "failed switch restores prior route")
	saved, err := fs.GetSetting(ctx, "upstream_proxy_url")
	require.NoError(t, err)
	require.Equal(t, "inherit", saved.Value, "failed switch preserves inherit instead of pinning the old port")
}

func TestSensitiveSettingMasksCanBeSubmittedWithoutOverwriting(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, log: nil}
	_, err := fs.SetSetting(ctx, "mail.smtp_password", domain.SettingTypeString, "smtp-secret")
	require.NoError(t, err)
	svc.reloadSettings(ctx)
	_, err = svc.UpdateSetting(ctx, "mail.smtp_password", "********")
	require.NoError(t, err)
	saved, err := fs.GetSetting(ctx, "mail.smtp_password")
	require.NoError(t, err)
	require.Equal(t, "smtp-secret", saved.Value)
}

func TestMaskedProxySettingPreservesAuthenticatedRoute(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, log: nil}
	_, err := fs.SetSetting(ctx, "upstream_proxy_url", domain.SettingTypeString, "socks5h://user:pass@fixture:7897")
	require.NoError(t, err)
	svc.SetUpstreamProxyApply(func(_ context.Context, raw string) error {
		require.Equal(t, "socks5h://user:pass@fixture:7897", raw)
		return nil
	})
	svc.reloadSettings(ctx)
	_, err = svc.UpdateSetting(ctx, "upstream_proxy_url", "socks5h://fixture:7897")
	require.NoError(t, err)
	saved, err := fs.GetSetting(ctx, "upstream_proxy_url")
	require.NoError(t, err)
	require.Equal(t, "socks5h://user:pass@fixture:7897", saved.Value)
}

func TestUpstreamProxyConcurrentUpdatesSerializeRollback(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, log: nil}
	svc.SetInitialUpstreamProxyURL("http://fixture:7892")
	// Seed the settings snapshot before installing the blocking hook. The
	// initial inherit route must not participate in the test interleaving.
	svc.reloadSettings(ctx)

	badEntered := make(chan struct{})
	releaseBad := make(chan struct{})
	goodApplied := make(chan struct{})
	var badOnce, goodOnce sync.Once
	svc.SetUpstreamProxyApply(func(_ context.Context, raw string) error {
		switch raw {
		case "http://fixture:bad":
			badOnce.Do(func() { close(badEntered) })
			<-releaseBad
			return context.DeadlineExceeded
		case "http://fixture:good":
			goodOnce.Do(func() { close(goodApplied) })
		}
		return nil
	})

	badDone := make(chan error, 1)
	go func() {
		_, err := svc.UpdateSetting(ctx, "upstream_proxy_url", "http://fixture:bad")
		badDone <- err
	}()
	<-badEntered

	goodDone := make(chan error, 1)
	go func() {
		_, err := svc.UpdateSetting(ctx, "upstream_proxy_url", "http://fixture:good")
		goodDone <- err
	}()

	// The second update must remain behind the first update's probe and
	// rollback. This watchdog is a scheduling check, not a sleep-based race.
	select {
	case <-goodApplied:
		t.Fatal("new proxy applied before the failed update completed")
	case <-time.After(250 * time.Millisecond):
	}
	close(releaseBad)

	require.ErrorIs(t, <-badDone, context.DeadlineExceeded)
	require.NoError(t, <-goodDone)
	require.Equal(t, "http://fixture:good", svc.UpstreamProxyURL())
	saved, err := fs.GetSetting(ctx, "upstream_proxy_url")
	require.NoError(t, err)
	require.Equal(t, "http://fixture:good", saved.Value)
}

// TestServiceTierPolicyKeysDerived P3-7：serviceTierPolicyKeys 从注册表
// PolicyValues 枚举域派生（消双处同步）——注册表是唯一事实源，派生表与注册表
// 一一对应（无残留手写 key）。
func TestServiceTierPolicyKeysDerived(t *testing.T) {
	want := map[string][]string{
		"service_tier_policy_priority": []string{"passthrough", "strip", "reject"},
		"service_tier_policy_flex":     []string{"passthrough", "strip", "reject"},
		"service_tier_policy_fast":     []string{"passthrough", "strip", "reject"},
	}

	for key, vals := range want {
		got, ok := serviceTierPolicyKeys[key]
		require.True(t, ok, "派生表含 %s", key)
		require.ElementsMatch(t, vals, got, "%s 值域随注册表", key)
	}
	// 派生表与注册表 PolicyValues 条目一一对应
	registered := 0
	for _, d := range domain.DefaultSettings {
		if len(d.PolicyValues) > 0 {
			registered++
			require.Contains(t, serviceTierPolicyKeys, d.Key, "注册表条目 %s 在派生表中", d.Key)
		}
	}
	require.Len(t, serviceTierPolicyKeys, registered, "派生表无注册表之外的 key")
}
