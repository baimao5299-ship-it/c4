// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/setting"
)

// SettingRepo 类型化配置持久化（key 唯一；缺省值由 domain.DefaultSettings
// 兜底，DB 无行即默认）。
type SettingRepo struct{ client *ent.Client }

// Get 取单 key 设置；DB 无行 → 默认值（注册开关等读路径免初始化）。
func (r *SettingRepo) Get(ctx context.Context, key string) (*domain.Setting, error) {
	row, err := r.client.Setting.Query().Where(setting.KeyEQ(key)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return domain.DefaultSetting(key), nil
		}
		return nil, err
	}
	return toDomainSetting(row), nil
}

// GetAll 全部设置（默认值 + DB 覆盖）。
func (r *SettingRepo) GetAll(ctx context.Context) ([]*domain.Setting, error) {
	rows, err := r.client.Setting.Query().Order(ent.Asc(setting.FieldKey)).All(ctx)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*domain.Setting, len(rows))
	for _, row := range rows {
		byKey[row.Key] = toDomainSetting(row)
	}
	out := make([]*domain.Setting, 0, len(domain.DefaultSettings)+len(rows))
	for _, d := range domain.DefaultSettings {
		if s, ok := byKey[d.Key]; ok {
			out = append(out, s)
		} else {
			dd := d
			out = append(out, &dd)
		}
	}
	return out, nil
}

// Set 设置（upsert：新 key 建行、已存在覆盖 value/type）。
func (r *SettingRepo) Set(ctx context.Context, key string, typ domain.SettingType, value string) (*domain.Setting, error) {
	_, err := r.client.Setting.Create().
		SetKey(key).
		SetType(setting.Type(typ)).
		SetValue(value).
		OnConflictColumns("key").
		Update(func(u *ent.SettingUpsert) {
			u.SetValue(value).
				SetType(setting.Type(typ)).
				SetUpdatedAt(time.Now())
		}).
		ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, key)
}
