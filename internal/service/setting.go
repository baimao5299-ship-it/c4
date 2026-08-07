package service

import (
	"context"
	"strconv"

	"go-proxy-mini/internal/domain"
)

// GetSettings 全部设置（默认值 + DB 覆盖；/admin/settings GET）。
func (s *Service) GetSettings(ctx context.Context) ([]*domain.Setting, error) {
	return s.store.GetAllSettings(ctx)
}

// UpdateSetting 类型化校验后更新（/admin/settings PUT）：
// key ∈ 内置注册表（未知 key → 400）；switch 必须 true/false；number 必须
// 数字。signup_enabled 即时生效——注册端点读 DB，无需缓存。
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
	return s.store.SetSetting(ctx, key, def.Type, value)
}
