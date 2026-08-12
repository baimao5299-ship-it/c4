// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/template"
)

func toDomainUser(u *ent.User) *domain.User {
	return &domain.User{
		ID: u.ID, Email: u.Email, PasswordHash: u.PasswordHash,
		Role: domain.Role(u.Role), Status: domain.UserStatus(u.Status),
		MaxConcurrency: u.MaxConcurrency, Balance: u.Balance,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func toDomainKey(k *ent.Key) *domain.Key {
	return &domain.Key{
		ID: k.ID, UserID: k.UserID, GroupID: k.GroupID, Name: k.Name,
		KeyHash: k.KeyHash, KeyPrefix: k.KeyPrefix,
		Status: domain.KeyStatus(k.Status), MaxConcurrency: k.MaxConcurrency,
		Quota: k.Quota, QuotaUsed: k.QuotaUsed,
		CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt, DeletedAt: k.DeletedAt,
	}
}

func toDomainGroup(g *ent.Group) *domain.Group {
	return &domain.Group{
		ID: g.ID, Name: g.Name, Visibility: domain.GroupVisibility(g.Visibility),
		PriceMultiplier: g.PriceMultiplier,
		ProtocolConvert: domain.ProtocolConvert(g.ProtocolConvert),
		CreatedAt:       g.CreatedAt, UpdatedAt: g.UpdatedAt, DeletedAt: g.DeletedAt,
	}
}

func toDomainTemplateExt(e *ent.TemplateExt) *domain.TemplateExt {
	return &domain.TemplateExt{
		TemplateID:      e.TemplateID,
		CredentialType:  credential.Type(e.CredentialType),
		StripImageTools: e.StripImageTools,
	}
}

func toDomainAccountExt(e *ent.AccountExt) *domain.AccountExt {
	return &domain.AccountExt{
		AccountID:         e.AccountID,
		CredentialType:    credential.Type(e.CredentialType),
		InstallationID:    e.InstallationID,
		SessionID:         e.SessionID,
		ThreadID:          e.ThreadID,
		WindowID:          e.WindowID,
		OAuthToken:        e.OauthToken,
		OAuthRefreshToken: e.OauthRefreshToken,
		OAuthExpiresAt:    e.OauthExpiresAt,
		PATKey:            e.PatKey,
		Email:             e.Email,
	}
}

func toDomainGroupAssignment(a *ent.GroupAssignment) *domain.GroupAssignment {
	return &domain.GroupAssignment{
		ID: a.ID, GroupID: a.GroupID, UserID: a.UserID,
		PriceMultiplier: a.PriceMultiplier,
		CreatedAt:       a.CreatedAt,
	}
}

func toDomainSetting(s *ent.Setting) *domain.Setting {
	return &domain.Setting{
		ID: s.ID, Key: s.Key, Type: domain.SettingType(s.Type),
		Value: s.Value, UpdatedAt: s.UpdatedAt,
	}
}

func toDomainTemplate(t *ent.Template) *domain.Template {
	formats := make([]domain.RequestFormat, 0, len(t.SupportedFormats))
	for _, f := range t.SupportedFormats {
		formats = append(formats, domain.RequestFormat(f))
	}
	fm := make(map[domain.RequestFormat][]string, len(t.FormatModels))
	for k, v := range t.FormatModels {
		fm[domain.RequestFormat(k)] = v
	}
	d := &domain.Template{
		ID: t.ID, Name: t.Name, BaseURL: t.BaseURL,
		CredentialType: credential.Type(t.CredentialType),
		SupportedFormats: formats, Models: t.Models,
		FormatModels: fm, ModelMapping: t.ModelMapping,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, DeletedAt: t.DeletedAt,
	}
	// StripImageTools 快照合并：仅调度器快照加载（LoadGroupsAccounts /
	// LoadGroupAccounts 的 WithTemplate 嵌套 WithExt）会 eager-load ext 边；
	// 其余路径（管理面模板 CRUD 等）无 ext 边 → nil → false。管理面 ext 配置
	// 经 template_ext 端点单独读写，不合并进模板对象。
	if len(t.Edges.Ext) > 0 && t.Edges.Ext[0].StripImageTools != nil {
		d.StripImageTools = *t.Edges.Ext[0].StripImageTools
	}
	return d
}

func toDomainAccount(a *ent.Account) *domain.Account {
	var tpl *domain.Template
	if a.Edges.Template != nil {
		tpl = toDomainTemplate(a.Edges.Template)
	}
	return &domain.Account{
		ID: a.ID, Name: a.Name, TemplateID: a.TemplateID, Template: tpl,
		UpstreamKey: a.UpstreamKey,
		Status: domain.AccountStatus(a.Status),
		CooldownUntil: a.CooldownUntil, Weight: a.Weight, MaxConcurrency: a.MaxConcurrency,
		LastError: a.LastError, LastUsedAt: a.LastUsedAt,
		FailedAt: a.FailedAt,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, DeletedAt: a.DeletedAt,
	}
}

// templatePredicate 供调用处过滤，避免未用 import 告警。
var _ = template.FieldName
