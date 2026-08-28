// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/pkg/httpx"
	"github.com/is7qin/c3api/pkg/logx"
)

const redactedSMTPPassword = "********"

// GetSettings 全部设置（默认值 + DB 覆盖；/api/admin/settings GET）。
func (s *Service) GetSettings(ctx context.Context) ([]*domain.Setting, error) {
	return s.store.GetAllSettings(ctx)
}

// serviceTierPolicyKeys service_tier 转发策略 key → 值域（P3-7：从注册表
// PolicyValues 枚举域派生，消双处同步——注册表是唯一事实源，新增策略 key 只改
// 注册表一处，此处随派生自动跟随；非法值 → 400，见 UpdateSetting）。
var serviceTierPolicyKeys = func() map[string][]string {
	m := make(map[string][]string, 3)
	for _, d := range domain.DefaultSettings {
		if len(d.PolicyValues) > 0 {
			m[d.Key] = d.PolicyValues
		}
	}
	return m
}()

// UpdateSetting 类型化校验后更新（/api/admin/settings PUT）：
// key ∈ 内置注册表（未知 key → 400）；switch 必须 true/false；number 必须
// 数字且落在注册表 Min/Max 值域内（负值/越界 → 400）；带 PolicyValues 枚举
// 域的条目（service_tier_policy_*）必须命中枚举。更新成功后同步内存快照——
// 注册等读路径即时生效；本地直连分发器按 scope 精确重载（#36 auth gate 预算
// 按新 N 即时重算）+ NOTIFY 广播其余实例。
func (s *Service) UpdateSetting(ctx context.Context, key, value string) (*domain.Setting, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// GET /api/admin/settings returns masked secrets. Submitting the unchanged
	// mask must preserve the stored value; an explicit new value still replaces
	// it normally.
	if key == "mail.smtp_password" && value == redactedSMTPPassword && s.settingValue(key) != "" {
		value = s.settingValue(key)
	}
	if key == "upstream_proxy_url" {
		previous := s.settingValue(key)
		if previous != "" && httpx.ProxySummary(previous) == value && previous != value {
			value = previous
		}
	}
	def := domain.DefaultSetting(key)
	if def == nil {
		return nil, ErrInvalidInput
	}
	switch def.Type {
	case domain.SettingTypeSwitch:
		if value != "true" && value != "false" {
			return nil, ErrInvalidInput
		}
	case domain.SettingTypeNumber:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, ErrInvalidInput
		}
		// 值域护栏（A-P2-11）：注册表 Min/Max 是单一事实源——越界 → 400，
		// 与管理面 CreateUser/UpdateUser 负值拒绝语义一致；消费端零改动。
		if def.Min != nil && n < *def.Min {
			return nil, ErrInvalidInput
		}
		if def.Max != nil && n > *def.Max {
			return nil, ErrInvalidInput
		}
	}
	if vals, ok := serviceTierPolicyKeys[key]; ok && !slices.Contains(vals, value) {
		return nil, ErrInvalidInput
	}
	// mail 依赖约束（fail-fast，无静默联动）：
	// register_verification 开启时要求 enabled=true 且 smtp_host/from_address 非空；
	// 关闭 enabled 时若 verif 仍为开同样拒绝。
	effective := func(k string) string {
		if k == key {
			return value
		}
		return s.settingValue(k)
	}
	if effective("mail.register_verification") == "true" {
		if effective("mail.enabled") != "true" || effective("mail.smtp_host") == "" || effective("mail.from_address") == "" {
			return nil, ErrInvalidInput
		}
	}
	proxyUpdate := key == "upstream_proxy_url"
	tlsUpdate := key == "codex_tls_convergence_enabled"
	runtimeUpdate := proxyUpdate || tlsUpdate
	runtimeLockHeld := runtimeUpdate
	if runtimeLockHeld {
		s.upstreamProxyUpdateMu.Lock()
	}
	proxyUnlock := func() {
		if runtimeLockHeld {
			s.upstreamProxyUpdateMu.Unlock()
			runtimeLockHeld = false
		}
	}
	defer proxyUnlock()
	previousValue := ""
	previousTLSExplicit := false
	if runtimeUpdate {
		// Keep the persisted value (inherit/direct/URL), not only its effective
		// URL. Rolling back the effective URL would silently turn an `inherit`
		// setting into a pinned port after one failed switch. TLS uses the same
		// runtime-apply transaction so a rejected combination never remains in
		// the database while the old transport is still active.
		previousValue = s.settingValue(key)
		if previousValue == "" && proxyUpdate {
			previousValue = "inherit"
		}
		if tlsUpdate {
			s.codexTLSMu.Lock()
			previousTLSExplicit = s.codexTLSExplicit
			s.codexTLSExplicit = true
			s.codexTLSMu.Unlock()
		}
	}
	set, err := s.store.SetSetting(ctx, key, def.Type, value)
	if err != nil {
		if tlsUpdate {
			s.codexTLSMu.Lock()
			s.codexTLSExplicit = previousTLSExplicit
			s.codexTLSMu.Unlock()
		}
		return nil, err
	}
	reload := func(reloadCtx context.Context) error {
		if runtimeLockHeld {
			return s.reloadSettingsLocked(reloadCtx)
		}
		return s.ReloadSettings(reloadCtx)
	}
	if err := reload(ctx); err != nil {
		// A runtime route/TLS change is committed only when its transport hook
		// accepts it. If parsing, probing, or compatibility validation fails,
		// restore the previous setting so the database and active route stay
		// aligned.
		if runtimeUpdate {
			rollback := previousValue
			// The admin request may already be canceled when the runtime apply
			// fails. Keep rollback independent of that request, but bounded so a
			// stalled database cannot leave the update goroutine blocked forever.
			rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			var rollbackErrs []error
			if _, rollbackErr := s.store.SetSetting(rollbackCtx, key, def.Type, rollback); rollbackErr != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("persist previous setting: %w", rollbackErr))
			}
			if reloadErr := s.reloadSettingsLocked(rollbackCtx); reloadErr != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("reload previous setting: %w", reloadErr))
			}
			rollbackCancel()
			if tlsUpdate {
				s.codexTLSMu.Lock()
				s.codexTLSExplicit = previousTLSExplicit
				s.codexTLSMu.Unlock()
			}
			if len(rollbackErrs) > 0 {
				return nil, fmt.Errorf("%w: %w (rollback: %v)", ErrInvalidInput, err, errors.Join(rollbackErrs...))
			}
		}
		if runtimeUpdate {
			return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
		return nil, err
	}
	// Do not hold the proxy update lock while the local dispatcher performs its
	// synchronous settings refresh; that callback calls ReloadSettings itself.
	proxyUnlock()
	// #36 本地实例即时重算（R2 M-1）：自播 NOTIFY 被 Listener Src 跳过，本地
	// settings 变更必须直连分发器——与远端 NOTIFY 同路径（Apply：同步
	// ReloadSettings + 注册表 ScopeSettings 精确重载 auth，gate 预算按新 N
	// 重算）。本地快照已由上方 reloadSettings 刷新，Apply 内 ReloadSettings
	// 是幂等重复（settings 低频路径，可接受；单一分发入口防本地/远端行为
	// 漂移）。30s 超时包裹本地直连链（合后清单：裸 WithoutCancel 无界——DB
	// 悬挂时 admin PUT 永久挂起、处理 goroutine 堆积；超时/请求取消中止本地
	// 收敛，由 NOTIFY/60s 周期兜底刷新补齐）。nil = 未装配 no-op。
	if s.local != nil {
		relCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		s.local.Apply(relCtx, notify.Change{Settings: true})
	}
	s.publish(ctx, notify.Change{Settings: true}) // 其余实例 settings 快照重载（#14 多实例）
	return set, nil
}

// ReloadSettings settings 快照全量重载（invalidate.SettingsReloader 接口实现，
// T3 main 装配注入 invalidate.Config.Settings；供 NOTIFY settings 分支触发全
// 实例重载）。与 UpdateSetting 内部路径同实现（reloadSettings 复用）；失败
// 返回错误由调用方（去抖器 reloadAll）Warn。
func (s *Service) ReloadSettings(ctx context.Context) error {
	s.upstreamProxyUpdateMu.Lock()
	defer s.upstreamProxyUpdateMu.Unlock()
	return s.reloadSettingsLocked(ctx)
}

// reloadSettingsLocked refreshes the settings snapshot and applies runtime
// settings. Callers must hold upstreamProxyUpdateMu when concurrent proxy
// updates must be serialized with this reload.
func (s *Service) reloadSettingsLocked(ctx context.Context) error {
	rows, err := s.store.GetAllSettings(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]*domain.Setting, len(rows))
	for _, st := range rows {
		m[st.Key] = st
	}
	s.settings.Store(&m)
	proxyErr := s.applyUpstreamProxy(ctx)
	tlsErr := s.applyCodexTLSConvergence()
	if proxyErr != nil {
		return proxyErr
	}
	return tlsErr
}

// SetInitialUpstreamProxyURL records the process-start fallback used when the
// persisted setting is "inherit". It does not touch the active transport.
func (s *Service) SetInitialUpstreamProxyURL(raw string) {
	s.upstreamProxyMu.Lock()
	s.upstreamProxyURL = raw
	s.upstreamProxyMu.Unlock()
}

// SetUpstreamProxyApply installs the live transport switch hook. The hook is
// called outside the service lock and must only publish a fully constructed
// route after it has passed its own connectivity check.
func (s *Service) SetUpstreamProxyApply(apply func(context.Context, string) error) {
	s.upstreamProxyMu.Lock()
	s.upstreamProxyApply = apply
	s.upstreamProxyMu.Unlock()
}

// UpstreamProxyURL returns the effective runtime URL without exposing any
// credentials. "inherit" means the immutable startup config; "direct" and an
// empty value mean direct mode.
func (s *Service) UpstreamProxyURL() string {
	raw := s.settingValue("upstream_proxy_url")
	s.upstreamProxyMu.RLock()
	initial := s.upstreamProxyURL
	s.upstreamProxyMu.RUnlock()
	if raw == "" || raw == "inherit" {
		return initial
	}
	if raw == "direct" {
		return ""
	}
	return raw
}

// ApplyUpstreamProxy applies the effective setting immediately. It is used by
// the composition root after the runtime switch hook is installed so a saved
// override is honored on startup without probing the configured fallback twice.
func (s *Service) ApplyUpstreamProxy(ctx context.Context) error {
	return s.applyUpstreamProxy(ctx)
}

func (s *Service) applyUpstreamProxy(ctx context.Context) error {
	s.upstreamProxyMu.RLock()
	apply := s.upstreamProxyApply
	s.upstreamProxyMu.RUnlock()
	if apply == nil {
		return nil
	}
	return apply(ctx, s.UpstreamProxyURL())
}

func (s *Service) SetCodexTLSConvergenceApply(apply func(bool) error) error {
	s.codexTLSMu.Lock()
	s.codexTLSConvergenceApply = apply
	s.codexTLSConvergenceReady = false
	s.codexTLSMu.Unlock()
	return s.applyCodexTLSConvergence()
}

// SetInitialCodexTLSConvergence records the process-start default. A persisted
// runtime setting with a real timestamp overrides it; an absent/default row
// keeps the startup configuration until an administrator explicitly changes it.
func (s *Service) SetInitialCodexTLSConvergence(enabled bool) {
	s.codexTLSMu.Lock()
	s.codexTLSInitial = enabled
	s.codexTLSInitialSet = true
	s.codexTLSMu.Unlock()
}

func (s *Service) applyCodexTLSConvergence() error {
	s.codexTLSMu.Lock()
	defer s.codexTLSMu.Unlock()
	desired := false
	if settings := s.settings.Load(); settings != nil {
		if st, ok := (*settings)["codex_tls_convergence_enabled"]; ok {
			desired = st.Value == "true"
			if s.codexTLSInitialSet && !s.codexTLSExplicit && st.UpdatedAt.IsZero() {
				desired = s.codexTLSInitial
			}
		} else if s.codexTLSInitialSet && !s.codexTLSExplicit {
			desired = s.codexTLSInitial
		}
	} else if s.codexTLSInitialSet && !s.codexTLSExplicit {
		desired = s.codexTLSInitial
	}
	apply := s.codexTLSConvergenceApply
	if apply == nil || (s.codexTLSConvergenceReady && s.codexTLSConvergenceValue == desired) {
		return nil
	}
	if err := apply(desired); err != nil {
		return err
	}
	s.codexTLSConvergenceReady = true
	s.codexTLSConvergenceValue = desired
	return nil
}

// reloadSettings 全量重载设置快照（New 初始化 + UpdateSetting 后调用）。
// 失败 fail-safe（评审 M-1）：仅告警，保留旧快照/空快照继续——读快照缺失
// 按零值处理（与无配置现状行为一致），不阻断服务启动。
func (s *Service) reloadSettings(ctx context.Context) {
	if err := s.ReloadSettings(ctx); err != nil && s.log != nil {
		s.log.Warn("settings snapshot reload failed", logx.Error(err))
	}
}

// settingValue 快照查值：缺失（含快照未初始化）返回空串。
func (s *Service) settingValue(key string) string {
	m := s.settings.Load()
	if m == nil {
		return ""
	}
	if st, ok := (*m)[key]; ok {
		return st.Value
	}
	return ""
}

// settingInt 快照数值读取：缺失/解析失败 → 0（UpdateSetting 已做类型化
// 校验，此处仅防御性兜底；解析失败按 0 = 不送/不限语义）。
func (s *Service) settingInt(key string) int64 {
	v, err := strconv.ParseInt(s.settingValue(key), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
