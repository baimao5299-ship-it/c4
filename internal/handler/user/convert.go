package user

import (
	"go-proxy-mini/internal/domain"
)

// redemptionValueToAPI 毫分存储 → 契约面值（与 /admin 兑换码同规则）：
// balance/temp_balance → USD（1 USD = 100,000 毫分）；concurrency → 并发数
// float64 直出（非货币，仅类型转换——整数精确）。
func redemptionValueToAPI(typ domain.RedemptionType, millis int64) float64 {
	if typ == domain.RedemptionTypeConcurrency {
		return float64(millis)
	}
	return float64(millis) / 1e5
}

// toAPIUser 用户领域对象 → 契约类型（口令散列永不下发；Balance 毫分 → USD
// 展示换算——1 USD = 100,000 毫分，与 /admin 用户端点同语义；价格倍率按组
// 挂载，User 无倍率字段）。
func toAPIUser(u *domain.User) User {
	r := UserRole(u.Role)
	st := UserStatus(u.Status)
	return User{
		ID:              &u.ID,
		Email:           &u.Email,
		Role:            &r,
		Status:          &st,
		MaxConcurrency:  &u.MaxConcurrency,
		Balance:         ptr(float64(u.Balance) / 1e5),
		CreatedAt:       &u.CreatedAt,
		UpdatedAt:       &u.UpdatedAt,
	}
}

// toAPIGroup 组领域对象 → 契约类型（/user/groups 只读列表；PriceMultiplier
// 万分数 → 正常值 float64，API 边界换算）。
func toAPIGroup(g *domain.Group) Group {
	v := GroupVisibility(g.Visibility)
	return Group{
		ID:              &g.ID,
		Name:            &g.Name,
		Visibility:      &v,
		PriceMultiplier: ptr(float64(g.PriceMultiplier) / 10000.0),
		CreatedAt:       &g.CreatedAt,
		UpdatedAt:       &g.UpdatedAt,
	}
}

// toAPIKey key 领域对象 → 契约类型（KeyHash 永不下发）。
func toAPIKey(k *domain.Key) Key {
	st := KeyStatus(k.Status)
	return Key{
		ID:             &k.ID,
		UserID:         &k.UserID,
		GroupID:        &k.GroupID,
		Name:           &k.Name,
		KeyPrefix:      &k.KeyPrefix,
		Status:         &st,
		MaxConcurrency: &k.MaxConcurrency,
		Quota:          &k.Quota,
		QuotaUsed:      &k.QuotaUsed,
		CreatedAt:      &k.CreatedAt,
		UpdatedAt:      &k.UpdatedAt,
	}
}

// toAPIUsageLog 用量日志领域对象 → 契约类型（/user/logs）。
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

// toAPIStatBucket 统计桶领域对象 → 契约类型（/user/stats）。
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
