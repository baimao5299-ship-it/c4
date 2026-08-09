package repository

import (
	"context"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/usagestat"
)

type StatQuery struct {
	GroupID   int64
	AccountID int64
	UserID    int64 // 0 = 不过滤（/user/stats 强制 = 自己）
	Model     string
	From      time.Time
	To        time.Time
}

type StatRepo struct{ client *ent.Client }

// Upsert 逐 bucket 冲突累加（规格 §10.5：聚合不可失真）。Cost 毫分
// （Phase 5 计费预聚合：统计面花费不扫明细）。
func (r *StatRepo) Upsert(ctx context.Context, buckets []*domain.StatBucket) error {
	for _, b := range buckets {
		_, err := r.client.UsageStat.Create().
			SetBucketTime(b.BucketTime).
			SetGroupID(b.GroupID).
			SetAccountID(b.AccountID).
			SetTemplateID(b.TemplateID).
			SetUserID(b.UserID).
			SetModel(b.Model).
			SetIsError(b.IsError).
			SetRequestCount(b.RequestCount).
			SetErrorCount(b.ErrorCount).
			SetInputTokens(b.InputTokens).
			SetOutputTokens(b.OutputTokens).
			SetTotalTokens(b.TotalTokens).
			SetCacheReadTokens(b.CacheReadTokens).
			SetCacheCreationTokens(b.CacheCreationTokens).
			SetCost(b.Cost).
			SetTotalLatencyMs(b.TotalLatencyMS).
			OnConflictColumns("bucket_time", "group_id", "account_id", "template_id", "user_id", "model", "is_error").
			Update(func(u *ent.UsageStatUpsert) {
				u.AddRequestCount(b.RequestCount)
				u.AddErrorCount(b.ErrorCount)
				u.AddInputTokens(b.InputTokens)
				u.AddOutputTokens(b.OutputTokens)
				u.AddTotalTokens(b.TotalTokens)
				u.AddCacheReadTokens(b.CacheReadTokens)
				u.AddCacheCreationTokens(b.CacheCreationTokens)
				u.AddCost(b.Cost)
				u.AddTotalLatencyMs(b.TotalLatencyMS)
			}).
			ID(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

// Scan 拉取时间范围内的原始小时桶（日聚合在 service 层做，规避方言差异）。
func (r *StatRepo) ScanStats(ctx context.Context, q StatQuery) ([]*domain.StatBucket, error) {
	pred := r.client.UsageStat.Query().
		Where(
			usagestat.BucketTimeGTE(q.From),
			usagestat.BucketTimeLT(q.To),
		)
	if q.GroupID > 0 {
		pred = pred.Where(usagestat.GroupIDEQ(q.GroupID))
	}
	if q.AccountID > 0 {
		pred = pred.Where(usagestat.AccountIDEQ(q.AccountID))
	}
	if q.UserID > 0 {
		pred = pred.Where(usagestat.UserIDEQ(q.UserID))
	}
	if q.Model != "" {
		pred = pred.Where(usagestat.ModelEQ(q.Model))
	}
	rows, err := pred.Order(ent.Asc(usagestat.FieldBucketTime)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.StatBucket, 0, len(rows))
	for _, row := range rows {
		out = append(out, &domain.StatBucket{
			BucketTime: row.BucketTime, GroupID: row.GroupID, AccountID: row.AccountID,
			TemplateID: row.TemplateID, UserID: row.UserID, Model: row.Model, IsError: row.IsError,
			RequestCount: row.RequestCount, ErrorCount: row.ErrorCount,
			InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			TotalTokens: row.TotalTokens, TotalLatencyMS: row.TotalLatencyMs,
			CacheReadTokens: row.CacheReadTokens, CacheCreationTokens: row.CacheCreationTokens,
			Cost: row.Cost,
		})
	}
	return out, nil
}
