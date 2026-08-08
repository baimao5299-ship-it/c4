package service

import (
	"context"
	"strconv"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

// GetSettings 全部设置（默认值 + DB 覆盖；/admin/settings GET）。
func (s *Service) GetSettings(ctx context.Context) ([]*domain.Setting, error) {
	return s.store.GetAllSettings(ctx)
}

// serviceTierPolicyKeys service_tier 转发策略设置（值域 passthrough/strip/reject，
// 见 domain.DefaultSettings 注释；非法值 → 400）。
var serviceTierPolicyKeys = map[string]bool{
	"service_tier_policy_priority": true,
	"service_tier_policy_flex":     true,
}

// UpdateSetting 类型化校验后更新（/admin/settings PUT）：
// key ∈ 内置注册表（未知 key → 400）；switch 必须 true/false；number 必须
// 数字；service_tier_policy_* 必须 passthrough/strip/reject。更新成功后同步
// 内存快照——注册等读路径即时生效。
func (s *Service) UpdateSetting(ctx context.Context, key, value string) (*domain.Setting, error) {
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
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return nil, ErrInvalidInput
		}
	}
	if serviceTierPolicyKeys[key] && value != "passthrough" && value != "strip" && value != "reject" {
		return nil, ErrInvalidInput
	}
	set, err := s.store.SetSetting(ctx, key, def.Type, value)
	if err != nil {
		return nil, err
	}
	s.reloadSettings(ctx)
	return set, nil
}

// reloadSettings 全量重载设置快照（New 初始化 + UpdateSetting 后调用）。
// 失败 fail-safe（评审 M-1）：仅告警，保留旧快照/空快照继续——读快照缺失
// 按零值处理（与无配置现状行为一致），不阻断服务启动。
func (s *Service) reloadSettings(ctx context.Context) {
	rows, err := s.store.GetAllSettings(ctx)
	if err != nil {
		if s.log != nil {
			s.log.Warn("settings snapshot reload failed", logx.Error(err))
		}
		return
	}
	m := make(map[string]*domain.Setting, len(rows))
	for _, st := range rows {
		m[st.Key] = st
	}
	s.settings.Store(&m)
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
