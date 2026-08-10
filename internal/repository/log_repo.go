package repository

import (
	"context"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/usagelog"
)

type LogQuery struct {
	GroupID    int64 // 0 = 不过滤
	AccountID  int64
	UserID     int64 // 0 = 不过滤（/user/logs 强制 = 自己）
	KeyID      int64
	Model      string
	StatusCode int
	ErrorType  string
	From       *time.Time
	To         *time.Time
	Offset     int
	Limit      int
}

type LogRepo struct{ client *ent.Client }

func (r *LogRepo) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	if len(logs) == 0 {
		return nil
	}
	builders := make([]*ent.UsageLogCreate, 0, len(logs))
	for _, l := range logs {
		builders = append(builders, buildUsageLogCreate(r.client, l))
	}
	_, err := r.client.UsageLog.CreateBulk(builders...).Save(ctx)
	return err
}

// buildUsageLogCreate 构建单条 usagelog 插入构建器（InsertBatch 与计费事务
// DeductAndLog 共用——tx client 经同一 client 传入即同一事务连接）。
// 计费列（Phase 5）：Cost 毫分（0 = 未计费/错误路径）；BillingTier 空 = 未计费
// 路径（落库 NULL）；AboveHit/Overdraft 布尔直接落。
// 时间/价格快照列（nil = NULL 落库，SQL 不写该列）：TTFTMS 首 token 时间毫秒
// （非流式/失败路径 nil）；Price*Millis 每 M token 毫分单价快照（未计费路径
// /无该分量 nil）。
func buildUsageLogCreate(client *ent.Client, l *domain.UsageLog) *ent.UsageLogCreate {
	c := client.UsageLog.Create().
		SetRequestID(l.RequestID).
		SetModel(l.Model).
		SetFormat(usagelog.Format(l.Format)).
		SetStatusCode(l.StatusCode).
		SetErrorType(string(l.ErrorType)).
		SetLatencyMs(l.LatencyMS).
		SetInputTokens(l.InputTokens).
		SetOutputTokens(l.OutputTokens).
		SetTotalTokens(l.TotalTokens).
		SetCacheReadTokens(l.CacheReadTokens).
		SetCacheCreationTokens(l.CacheCreationTokens).
		SetCost(l.Cost).
		SetAboveHit(l.AboveHit).
		SetOverdraft(l.Overdraft).
		SetCreatedAt(l.CreatedAt)
	if l.GroupID > 0 {
		c = c.SetGroupID(l.GroupID)
	}
	if l.AccountID > 0 {
		c = c.SetAccountID(l.AccountID)
	}
	if l.TemplateID > 0 {
		c = c.SetTemplateID(l.TemplateID)
	}
	if l.UserID > 0 {
		c = c.SetUserID(l.UserID)
	}
	if l.KeyID > 0 {
		c = c.SetKeyID(l.KeyID)
	}
	if l.MappedModel != "" {
		c = c.SetMappedModel(l.MappedModel)
	}
	if l.ErrorMessage != nil {
		c = c.SetErrorMessage(*l.ErrorMessage)
	}
	if l.BillingTier != "" {
		c = c.SetBillingTier(l.BillingTier)
	}
	if l.TTFTMS != nil {
		c = c.SetTtftMs(*l.TTFTMS)
	}
	if l.PriceInputMillis != nil {
		c = c.SetPriceInputMillis(*l.PriceInputMillis)
	}
	if l.PriceOutputMillis != nil {
		c = c.SetPriceOutputMillis(*l.PriceOutputMillis)
	}
	if l.PriceCacheReadMillis != nil {
		c = c.SetPriceCacheReadMillis(*l.PriceCacheReadMillis)
	}
	if l.PriceCacheCreationMillis != nil {
		c = c.SetPriceCacheCreationMillis(*l.PriceCacheCreationMillis)
	}
	return c
}

func (r *LogRepo) QueryLogs(ctx context.Context, q LogQuery) ([]*domain.UsageLog, int64, error) {
	pred := r.client.UsageLog.Query()
	if q.GroupID > 0 {
		pred = pred.Where(usagelog.GroupIDEQ(q.GroupID))
	}
	if q.AccountID > 0 {
		pred = pred.Where(usagelog.AccountIDEQ(q.AccountID))
	}
	if q.UserID > 0 {
		pred = pred.Where(usagelog.UserIDEQ(q.UserID))
	}
	if q.KeyID > 0 {
		pred = pred.Where(usagelog.KeyIDEQ(q.KeyID))
	}
	if q.Model != "" {
		pred = pred.Where(usagelog.ModelEQ(q.Model))
	}
	if q.StatusCode > 0 {
		pred = pred.Where(usagelog.StatusCodeEQ(q.StatusCode))
	}
	if q.ErrorType != "" {
		pred = pred.Where(usagelog.ErrorTypeEQ(q.ErrorType))
	}
	if q.From != nil {
		pred = pred.Where(usagelog.CreatedAtGTE(*q.From))
	}
	if q.To != nil {
		pred = pred.Where(usagelog.CreatedAtLTE(*q.To))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	rows, err := pred.Order(ent.Desc(usagelog.FieldID)).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.UsageLog, 0, len(rows))
	for _, row := range rows {
		l := &domain.UsageLog{
			ID: row.ID, RequestID: row.RequestID,
			Model: row.Model, Format: domain.RequestFormat(row.Format),
			StatusCode: row.StatusCode, ErrorType: domain.ErrorType(row.ErrorType),
			ErrorMessage: row.ErrorMessage,
			LatencyMS:                row.LatencyMs,
			TTFTMS:                   row.TtftMs,
			InputTokens:              row.InputTokens,
			PriceInputMillis:         row.PriceInputMillis,
			OutputTokens:             row.OutputTokens,
			PriceOutputMillis:        row.PriceOutputMillis,
			TotalTokens:              row.TotalTokens,
			CacheReadTokens:          row.CacheReadTokens,
			PriceCacheReadMillis:     row.PriceCacheReadMillis,
			CacheCreationTokens:      row.CacheCreationTokens,
			PriceCacheCreationMillis: row.PriceCacheCreationMillis,
			Cost:                row.Cost,
			AboveHit:            row.AboveHit,
			Overdraft:           row.Overdraft,
			CreatedAt:           row.CreatedAt,
		}
		if row.GroupID != nil {
			l.GroupID = *row.GroupID
		}
		if row.AccountID != nil {
			l.AccountID = *row.AccountID
		}
		if row.TemplateID != nil {
			l.TemplateID = *row.TemplateID
		}
		if row.UserID != nil {
			l.UserID = *row.UserID
		}
		if row.KeyID != nil {
			l.KeyID = *row.KeyID
		}
		if row.MappedModel != nil {
			l.MappedModel = *row.MappedModel
		}
		if row.BillingTier != nil {
			l.BillingTier = *row.BillingTier
		}
		out = append(out, l)
	}
	return out, int64(total), nil
}
