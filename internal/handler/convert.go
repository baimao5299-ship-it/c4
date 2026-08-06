package handler

import (
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/service"
)

// 本文件实现领域类型 → 生成的契约类型（api.gen.go）的转换。
// oapi-codegen v2.4.1 对非 required 属性生成指针 + omitempty 字段
// （如 ID *int64、Name *string），因此响应字段全部经取址赋值；
// 枚举类型（RequestFormat/AccountStatus/ErrorType）跨包需显式转换。

// toAPITemplate 模板领域对象 → 契约类型。
func toAPITemplate(t *domain.Template) Template {
	f := RequestFormat(t.DefaultFormat)
	return Template{
		ID:            &t.ID,
		Name:          &t.Name,
		BaseURL:       &t.BaseURL,
		DefaultFormat: &f,
		Models:        &t.Models,
		ModelFormats:  toAPITemplateFormats(t.ModelFormats),
		ModelMapping:  &t.ModelMapping,
		CreatedAt:     &t.CreatedAt,
		UpdatedAt:     &t.UpdatedAt,
	}
}

// toAPIAccount 账号领域对象 → 契约类型（Template 字段由仓库预加载）。
func toAPIAccount(a *domain.Account) Account {
	st := AccountStatus(a.Status)
	var tpl *Template
	if a.Template != nil {
		t := toAPITemplate(a.Template)
		tpl = &t
	}
	return Account{
		ID:             &a.ID,
		Name:           &a.Name,
		TemplateID:     &a.TemplateID,
		Template:       tpl,
		UpstreamKey:    &a.UpstreamKey,
		Status:         &st,
		CooldownUntil:  a.CooldownUntil,
		Weight:         &a.Weight,
		MaxConcurrency: &a.MaxConcurrency,
		LastError:      a.LastError,
		LastUsedAt:     a.LastUsedAt,
		CreatedAt:      &a.CreatedAt,
		UpdatedAt:      &a.UpdatedAt,
	}
}

// toAPIAccountView 账号运行时视图 → 契约类型（AccountView 是平铺结构，
// 非 allOf 嵌入：所有 Account 字段内联 + concurrency/err_rate/err_count）。
func toAPIAccountView(v *service.AccountView) AccountView {
	base := toAPIAccount(v.Account)
	return AccountView{
		ID:             base.ID,
		Name:           base.Name,
		TemplateID:     base.TemplateID,
		Template:       base.Template,
		UpstreamKey:    base.UpstreamKey,
		Status:         base.Status,
		CooldownUntil:  base.CooldownUntil,
		Weight:         base.Weight,
		MaxConcurrency: base.MaxConcurrency,
		LastError:      base.LastError,
		LastUsedAt:     base.LastUsedAt,
		CreatedAt:      base.CreatedAt,
		UpdatedAt:      base.UpdatedAt,
		Concurrency:    &v.Concurrency,
		ErrRate:        &v.ErrRate,
		ErrCount:       &v.ErrCount,
	}
}

// toAPIGroup 分组领域对象 → 契约类型。
func toAPIGroup(g *domain.Group) Group {
	return Group{
		ID:        &g.ID,
		Name:      &g.Name,
		KeyHash:   &g.KeyHash,
		KeyPrefix: &g.KeyPrefix,
		CreatedAt: &g.CreatedAt,
		UpdatedAt: &g.UpdatedAt,
	}
}

// toAPIUsageLog 用量日志领域对象 → 契约类型。
func toAPIUsageLog(l *domain.UsageLog) UsageLog {
	f := RequestFormat(l.Format)
	et := ErrorType(l.ErrorType)
	return UsageLog{
		ID:               &l.ID,
		RequestID:        &l.RequestID,
		GroupID:          &l.GroupID,
		AccountID:        &l.AccountID,
		TemplateID:       &l.TemplateID,
		Model:            &l.Model,
		MappedModel:      &l.MappedModel,
		Format:           &f,
		StatusCode:       &l.StatusCode,
		ErrorType:        &et,
		LatencyMS:        &l.LatencyMS,
		PromptTokens:     &l.PromptTokens,
		CompletionTokens: &l.CompletionTokens,
		TotalTokens:      &l.TotalTokens,
		CreatedAt:        &l.CreatedAt,
	}
}

// toAPIStatBucket 统计桶领域对象 → 契约类型。
func toAPIStatBucket(b *domain.StatBucket) StatBucket {
	return StatBucket{
		BucketTime:       &b.BucketTime,
		GroupID:          &b.GroupID,
		AccountID:        &b.AccountID,
		TemplateID:       &b.TemplateID,
		Model:            &b.Model,
		IsError:          &b.IsError,
		RequestCount:     &b.RequestCount,
		ErrorCount:       &b.ErrorCount,
		PromptTokens:     &b.PromptTokens,
		CompletionTokens: &b.CompletionTokens,
		TotalTokens:      &b.TotalTokens,
		TotalLatencyMS:   &b.TotalLatencyMS,
	}
}

// toAPITemplateFormats 把领域 map（值类型为 domain.RequestFormat）
// 转换为契约 map（值类型为 RequestFormat）；nil 输入产出指向 nil map
// 的指针（wire 上仍为 "ModelFormats":null，与旧实现一致）。
func toAPITemplateFormats(m map[string]domain.RequestFormat) *map[string]RequestFormat {
	var out map[string]RequestFormat
	if m != nil {
		out = make(map[string]RequestFormat, len(m))
		for k, v := range m {
			out[k] = RequestFormat(v)
		}
	}
	return &out
}
