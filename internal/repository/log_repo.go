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
		c := r.client.UsageLog.Create().
			SetRequestID(l.RequestID).
			SetModel(l.Model).
			SetFormat(usagelog.Format(l.Format)).
			SetStatusCode(l.StatusCode).
			SetErrorType(string(l.ErrorType)).
			SetLatencyMs(l.LatencyMS).
			SetPromptTokens(l.PromptTokens).
			SetCompletionTokens(l.CompletionTokens).
			SetTotalTokens(l.TotalTokens).
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
		if l.MappedModel != "" {
			c = c.SetMappedModel(l.MappedModel)
		}
		builders = append(builders, c)
	}
	_, err := r.client.UsageLog.CreateBulk(builders...).Save(ctx)
	return err
}

func (r *LogRepo) QueryLogs(ctx context.Context, q LogQuery) ([]*domain.UsageLog, int64, error) {
	pred := r.client.UsageLog.Query()
	if q.GroupID > 0 {
		pred = pred.Where(usagelog.GroupIDEQ(q.GroupID))
	}
	if q.AccountID > 0 {
		pred = pred.Where(usagelog.AccountIDEQ(q.AccountID))
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
			LatencyMS:        row.LatencyMs,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			TotalTokens:      row.TotalTokens,
			CreatedAt:        row.CreatedAt,
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
		if row.MappedModel != nil {
			l.MappedModel = *row.MappedModel
		}
		out = append(out, l)
	}
	return out, int64(total), nil
}

func (r *LogRepo) PurgeLogs(ctx context.Context, olderThan time.Time) error {
	_, err := r.client.UsageLog.Delete().Where(usagelog.CreatedAtLT(olderThan)).Exec(ctx)
	return err
}
