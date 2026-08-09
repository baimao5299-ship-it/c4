package repository

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"

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

type StatRepo struct {
	client *ent.Client
	// driver 为 raw SQL（批量 Upsert 单语句）用：普通 client 与 tx client
	// （WithTx 内）均可用——ent v0.14 生成代码无 ExecContext，raw SQL 经
	// dialect.Driver 统一执行（对齐 pricing_repo/billing_repo 先例）。
	driver dialect.Driver
}

// statUpsertCols usage_stats 批量 upsert 列（与 schema 序一致；updated_at
// NOT NULL 无默认，行值 = time.Now()）。
var statUpsertCols = []string{
	"bucket_time", "group_id", "account_id", "template_id", "user_id", "model", "is_error",
	"request_count", "error_count", "prompt_tokens", "completion_tokens", "total_tokens",
	"cache_read_tokens", "cache_creation_tokens", "cost", "total_latency_ms", "updated_at",
}

// statUpsertMeasureCols DO UPDATE SET 的冲突累加列（测量列；维度列只作
// ON CONFLICT 目标，不做加法——见 Upsert 注释）。
var statUpsertMeasureCols = []string{
	"request_count", "error_count", "prompt_tokens", "completion_tokens", "total_tokens",
	"cache_read_tokens", "cache_creation_tokens", "cost", "total_latency_ms",
}

// Upsert 批量 upsert + 冲突累加（规格 §10.5：聚合不可失真）——单条 SQL 替代
// 逐 bucket 轮询（#15 验收：45k/s 下桶基数≈每请求、10k 桶逐 key Upsert 是
// 统计面慢 flush 3-5min 周期根因之一）。Cost 毫分（Phase 5 计费预聚合：
// 统计面花费不扫明细）。调用方（usage.Recorder.flushStats）按 statBatchSize
// 分块并以块为失败回灌原子单位——本方法单语句全成或全败，勿在外部合并
// 部分成功块（会重复计数）。
func (r *StatRepo) Upsert(ctx context.Context, buckets []*domain.StatBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(`INSERT INTO "usage_stats" (`)
	for i, col := range statUpsertCols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"`)
		b.WriteString(col)
		b.WriteString(`"`)
	}
	b.WriteString(") VALUES ")
	args := make([]any, 0, len(buckets)*len(statUpsertCols))
	now := time.Now()
	for i, bucket := range buckets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j := range statUpsertCols {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString("$")
			b.WriteString(strconv.Itoa(len(args) + 1))
			if j == 0 {
				args = append(args, bucket.BucketTime)
			} else if j == len(statUpsertCols)-1 {
				args = append(args, now) // updated_at
			} else {
				args = append(args, statColValue(bucket, statUpsertCols[j]))
			}
		}
		b.WriteByte(')')
	}
	b.WriteString(` ON CONFLICT ("bucket_time", "group_id", "account_id", "template_id", "user_id", "model", "is_error") DO UPDATE SET `)
	// 冲突累加：**只对测量列**做加法（增量语义，聚合并行/重试安全）——维度列
	// （bucket_time/group_id/account_id/template_id/user_id/model/is_error）只
	// 在 ON CONFLICT 目标里，不进 SET：varchar 相加 42883（model 维度列——
	// 压测实证统计批量 upsert 零落库根因）、bigint 维度列相加会翻倍（ID 值
	// 失真）。
	for i, col := range statUpsertMeasureCols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"`)
		b.WriteString(col)
		b.WriteString(`" = "usage_stats"."`)
		b.WriteString(col)
		b.WriteString(`" + "excluded"."`)
		b.WriteString(col)
		b.WriteString(`"`)
	}
	b.WriteString(`, "updated_at" = "excluded"."updated_at"`)
	var res sql.Result
	return r.driver.Exec(ctx, b.String(), args, &res)
}

func statColValue(b *domain.StatBucket, col string) any {
	switch col {
	case "group_id":
		return b.GroupID
	case "account_id":
		return b.AccountID
	case "template_id":
		return b.TemplateID
	case "user_id":
		return b.UserID
	case "model":
		return b.Model
	case "is_error":
		return b.IsError
	case "request_count":
		return b.RequestCount
	case "error_count":
		return b.ErrorCount
	case "prompt_tokens":
		return b.PromptTokens
	case "completion_tokens":
		return b.CompletionTokens
	case "total_tokens":
		return b.TotalTokens
	case "cache_read_tokens":
		return b.CacheReadTokens
	case "cache_creation_tokens":
		return b.CacheCreationTokens
	case "cost":
		return b.Cost
	case "total_latency_ms":
		return b.TotalLatencyMS
	}
	panic("stat repo: unknown column " + col)
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
			PromptTokens: row.PromptTokens, CompletionTokens: row.CompletionTokens,
			TotalTokens: row.TotalTokens, TotalLatencyMS: row.TotalLatencyMs,
			CacheReadTokens: row.CacheReadTokens, CacheCreationTokens: row.CacheCreationTokens,
			Cost: row.Cost,
		})
	}
	return out, nil
}
