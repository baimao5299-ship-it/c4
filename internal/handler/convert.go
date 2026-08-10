package handler

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/service"
)

// 本文件实现领域类型 → 生成的契约类型（api.gen.go）的转换。
// oapi-codegen v2.4.1 对 required 属性生成值类型、非 required 生成指针 +
// omitempty 字段，因此响应字段按生成类型取址/取值赋值；
// 枚举类型（RequestFormat/AccountStatus/ErrorType）跨包需显式转换。

// toAPITemplate 模板领域对象 → 契约类型。
func toAPITemplate(t *domain.Template) Template {
	formats := make([]TemplateSupportedFormats, 0, len(t.SupportedFormats))
	for _, f := range t.SupportedFormats {
		formats = append(formats, TemplateSupportedFormats(f))
	}
	return Template{
		ID:               t.ID,
		Name:             t.Name,
		BaseURL:          t.BaseURL,
		CredentialType:   ptr(string(t.CredentialType)),
		SupportedFormats: formats,
		Models:           &t.Models,
		FormatModels:     toAPITemplateFormatModels(t.FormatModels),
		ModelMapping:     &t.ModelMapping,
		CreatedAt:        t.CreatedAt,
		UpdatedAt:        t.UpdatedAt,
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

// toAPIGroup 分组领域对象 → 契约类型（PriceMultiplier 万分数 → 正常值
// float64，与 balance 毫分↔USD 同构的 API 边界换算）。
func toAPIGroup(g *domain.Group) Group {
	v := GroupVisibility(g.Visibility)
	return Group{
		ID:              &g.ID,
		Name:            &g.Name,
		Visibility:      &v,
		PriceMultiplier: ptr(multToNormal(g.PriceMultiplier)),
		CreatedAt:       &g.CreatedAt,
		UpdatedAt:       &g.UpdatedAt,
	}
}

// multToNormal 万分数 → 正常值（API 展示换算：15000 → 1.5；1 USD = 100,000
// 毫分同构——内部存储恒万分数，仅 API 边界换算）。
func multToNormal(v int) float64 { return float64(v) / 10000.0 }

// normalToMult 正常值 → 万分数（API 输入换算：1.5 → 15000；math.Round 消除
// 浮点取整误差）。
func normalToMult(v float64) int { return int(math.Round(v * 10000)) }

// apiMultiplierToMillis 生成类型倍率（正常值 *float64，nil = 未指定）→ 万分数
// *int；越界（<0 或 >10）→ 错误（400 文案）。
func apiMultiplierToMillis(v *float64) (*int, error) {
	if v == nil {
		return nil, nil
	}
	if *v < 0 || *v > 10 {
		return nil, errors.New("price_multiplier must be in [0, 10]")
	}
	m := normalToMult(*v)
	return &m, nil
}

// apiMultiplierMap 生成类型 multipliers（map[string]*float64，key = user_id /
// group_id 字符串）→ 万分数 map[int64]*int（nil 值 = 清除为未设置）；key 非法/
// 越界 → 错误。
func apiMultiplierMap(in map[string]*float64) (map[int64]*int, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[int64]*int, len(in))
	for k, v := range in {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("multipliers: invalid key %q", k)
		}
		if v == nil {
			out[id] = nil // null = 清除为未设置
			continue
		}
		if *v < 0 || *v > 10 {
			return nil, fmt.Errorf("multipliers: price_multiplier must be in [0, 10] for id %d", id)
		}
		m := normalToMult(*v)
		out[id] = &m
	}
	return out, nil
}

// toAPIMultipliers 万分数 map[int64]*int → 契约类型 map[string]*float64（正常
// 值；nil = 未设置 → null）。nil 输入 → nil（omitempty 缺省不输出）。
func toAPIMultipliers(m map[int64]*int) *map[string]*float64 {
	if m == nil {
		return nil
	}
	out := make(map[string]*float64, len(m))
	for uid, v := range m {
		if v == nil {
			out[strconv.FormatInt(uid, 10)] = nil
			continue
		}
		f := multToNormal(*v)
		out[strconv.FormatInt(uid, 10)] = &f
	}
	return &out
}

// millisToUSD 毫分 → USD float64（1 USD = 100,000 毫分；handler 边界展示换算，
// 内部存储恒毫分）。
func millisToUSD(millis int64) float64 { return float64(millis) / 1e5 }

// usdToMillis USD float64 → 毫分（math.Round 消除浮点取整误差；API 输入边界
// 换算，内部存储恒毫分）。
func usdToMillis(usd float64) int64 { return int64(math.Round(usd * 1e5)) }

// millisToUSDPtr *int64（毫分）→ *float64（USD）；nil 透传（价格矩阵可选列）。
func millisToUSDPtr(v *int64) *float64 {
	if v == nil {
		return nil
	}
	f := millisToUSD(*v)
	return &f
}

// usdToMillisPtr *float64（USD）→ *int64（毫分）；nil 透传（缺省 = 清空该价）。
func usdToMillisPtr(v *float64) *int64 {
	if v == nil {
		return nil
	}
	i := usdToMillis(*v)
	return &i
}

// multI64ToNormalPtr *int64（万分数）→ *float64（正常值）；nil 透传（pricing
// 矩阵 FastMultiplier 列，int64 存储）。
func multI64ToNormalPtr(v *int64) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v) / 10000.0
	return &f
}

// normalToMultI64Ptr *float64（正常值）→ *int64（万分数）；nil 透传。
func normalToMultI64Ptr(v *float64) *int64 {
	if v == nil {
		return nil
	}
	i := int64(math.Round(*v * 10000))
	return &i
}

// toAPIUser 用户领域对象 → 契约类型（PasswordHash 永不下发；Balance 毫分 →
// USD 展示换算；价格倍率按组（T3.5 修正）挂在 group_assignment 上，User 无
// 倍率字段）。
func toAPIUser(u *domain.User) User {
	r := UserRole(u.Role)
	st := UserStatus(u.Status)
	return User{
		ID:              &u.ID,
		Email:           &u.Email,
		Role:            &r,
		Status:          &st,
		MaxConcurrency:  &u.MaxConcurrency,
		Balance:         ptr(millisToUSD(u.Balance)),
		CreatedAt:       &u.CreatedAt,
		UpdatedAt:       &u.UpdatedAt,
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
		UserID:           &l.UserID,
		KeyID:            &l.KeyID,
		Model:            &l.Model,
		MappedModel:      &l.MappedModel,
		Format:           &f,
		StatusCode:       &l.StatusCode,
		ErrorType:        &et,
		LatencyMS:                &l.LatencyMS,
		TTFTMS:                   l.TTFTMS,
		InputTokens:              &l.InputTokens,
		PriceInputMillis:         l.PriceInputMillis,
		OutputTokens:             &l.OutputTokens,
		PriceOutputMillis:        l.PriceOutputMillis,
		TotalTokens:              &l.TotalTokens,
		CacheReadTokens:          &l.CacheReadTokens,
		PriceCacheReadMillis:     l.PriceCacheReadMillis,
		CacheCreationTokens:      &l.CacheCreationTokens,
		PriceCacheCreationMillis: l.PriceCacheCreationMillis,
		Cost:                &l.Cost,
		BillingTier:         &l.BillingTier,
		AboveHit:            &l.AboveHit,
		Overdraft:           &l.Overdraft,
		CreatedAt:           &l.CreatedAt,
	}
}

// toAPIStatBucket 统计桶领域对象 → 契约类型。
func toAPIStatBucket(b *domain.StatBucket) StatBucket {
	return StatBucket{
		BucketTime:       &b.BucketTime,
		GroupID:          &b.GroupID,
		AccountID:        &b.AccountID,
		TemplateID:       &b.TemplateID,
		UserID:           &b.UserID,
		Model:            &b.Model,
		IsError:          &b.IsError,
		RequestCount:     &b.RequestCount,
		ErrorCount:       &b.ErrorCount,
		InputTokens:         &b.InputTokens,
		OutputTokens:        &b.OutputTokens,
		TotalTokens:         &b.TotalTokens,
		CacheReadTokens:     &b.CacheReadTokens,
		CacheCreationTokens: &b.CacheCreationTokens,
		Cost:                &b.Cost,
		TotalLatencyMS:      &b.TotalLatencyMS,
	}
}

// toAPISetting 设置领域对象 → 契约类型。
func toAPISetting(s *domain.Setting) Setting {
	t := SettingType(s.Type)
	return Setting{
		Key:       &s.Key,
		Type:      &t,
		Value:     &s.Value,
		UpdatedAt: &s.UpdatedAt,
	}
}

// toAPITemplateFormatModels 把领域 map（键为 domain.RequestFormat）转换为契约
// map（键为 string）；nil 输入产出指向 nil map 的指针（wire 上仍为 "FormatModels":null）。
func toAPITemplateFormatModels(m map[domain.RequestFormat][]string) *map[string][]string {
	var out map[string][]string
	if m != nil {
		out = make(map[string][]string, len(m))
		for k, v := range m {
			out[string(k)] = v
		}
	}
	return &out
}
