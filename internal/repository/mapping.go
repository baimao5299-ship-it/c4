package repository

import (
	"go-proxy-mini/internal/credential"
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/template"
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
	return &domain.Template{
		ID: t.ID, Name: t.Name, BaseURL: t.BaseURL,
		CredentialType: credential.Type(t.CredentialType),
		SupportedFormats: formats, Models: t.Models,
		FormatModels: fm, ModelMapping: t.ModelMapping,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, DeletedAt: t.DeletedAt,
	}
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
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, DeletedAt: a.DeletedAt,
	}
}

// templatePredicate 供调用处过滤，避免未用 import 告警。
var _ = template.FieldName
