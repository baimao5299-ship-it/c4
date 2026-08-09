package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// StatRepo 统计仓库：查询经 ent client；批量写入（Upsert）走 pgx 原生连接池
// COPY 两阶段（#17 治本：消除 8.5k 参数 raw VALUES 大事务）。pool 由
// NewWithPG 构造注入（生产 main.go 注入 OpenPG 池；与 ent driver 同 DSN 共享
// 连接上限）。
type StatRepo struct {
	client *ent.Client
	pool   *pgxpool.Pool
}

// statUpsertCols usage_stats 批量 upsert 列（与 schema 序一致；updated_at
// NOT NULL 无默认，行值 = time.Now()）。
var statUpsertCols = []string{
	"bucket_time", "group_id", "account_id", "template_id", "user_id", "model", "is_error",
	"request_count", "error_count", "input_tokens", "output_tokens", "total_tokens",
	"cache_read_tokens", "cache_creation_tokens", "cost", "total_latency_ms", "updated_at",
}

// statUpsertMeasureCols DO UPDATE SET 的冲突累加列（测量列；维度列只作
// ON CONFLICT 目标，不做加法——见 Upsert 注释）。
var statUpsertMeasureCols = []string{
	"request_count", "error_count", "input_tokens", "output_tokens", "total_tokens",
	"cache_read_tokens", "cache_creation_tokens", "cost", "total_latency_ms",
}

// statCopyTable COPY 第一阶段临时表：会话级 + ON COMMIT DROP——事务提交/回滚
// 即消失，连接回池复用不留残表；并发 Upsert（flushStats 多 worker）各自独立
// 会话/事务，同名互不干扰。
var statCopyTable = pgx.Identifier{"usage_stats_copy"}

// statCopyCreateSQL 临时表列型与 usage_stats 一致（列序 = statUpsertCols；
// COPY 行值经 pgx 原生类型映射：time.Time→timestamptz、int64→bigint、
// string→varchar、bool→boolean，不做字符串拼接）。
var statCopyCreateSQL = `CREATE TEMP TABLE "usage_stats_copy" (
	"bucket_time" timestamptz,
	"group_id" bigint,
	"account_id" bigint,
	"template_id" bigint,
	"user_id" bigint,
	"model" varchar,
	"is_error" boolean,
	"request_count" bigint,
	"error_count" bigint,
	"input_tokens" bigint,
	"output_tokens" bigint,
	"total_tokens" bigint,
	"cache_read_tokens" bigint,
	"cache_creation_tokens" bigint,
	"cost" bigint,
	"total_latency_ms" bigint,
	"updated_at" timestamptz
) ON COMMIT DROP`

// statCopyMergeSQL 第二阶段合并（单条 SQL，构建一次复用）：临时表整表与
// usage_stats 按维度列冲突合并——已存在 → 测量列累加 + updated_at 刷新；
// 不存在 → 直接插入（excluded = 本次待插入行，INSERT..SELECT 同语义）。
var statCopyMergeSQL = func() string {
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
	b.WriteString(`) SELECT `)
	for i, col := range statUpsertCols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"`)
		b.WriteString(col)
		b.WriteString(`"`)
	}
	b.WriteString(` FROM "usage_stats_copy" ON CONFLICT ("bucket_time", "group_id", "account_id", "template_id", "user_id", "model", "is_error") DO UPDATE SET `)
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
	return b.String()
}()

// Upsert 批量 upsert + 冲突累加（规格 §10.5：聚合不可失真）——COPY 两阶段
//（#17：raw SQL 批量 upsert 单条 8.5k 参数大事务秒级占用 DB 连接，与 billing
// flusher 争连接池 → 压测 -12% 吞吐回归；治本后连接占用毫秒级）：
//  ① Acquire 单连接 → Begin → CREATE TEMP TABLE → pgx CopyFrom 原生批量
//    （数据不经 SQL 解析/参数编码，走 pgx 类型映射）
//  ② INSERT INTO usage_stats SELECT ... FROM temp ON CONFLICT ... DO UPDATE
//    累加（单条 SQL，PG 内部哈希 join 合并；无客户端参数拼接）
// 临时表/COPY/合并必须在同一连接同一事务（CREATE TEMP TABLE 会话级，pool
// 连接复用下跨事务即残表）→ 单连接事务内完成，Commit 后 Release。
// 失败语义：任一步失败 → 整体回滚 → 返回 error（chunk 原子回灌由调用方
// usage.Recorder.flushStats 处理：失败 chunk 连同其后剩余合并回灌，不丢不重）。
// Cost 毫分（Phase 5 计费预聚合：统计面花费不扫明细）。
func (r *StatRepo) Upsert(ctx context.Context, buckets []*domain.StatBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	if r.pool == nil {
		return fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot COPY upsert %d buckets", len(buckets))
	}
	now := time.Now()
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // nolint:errcheck // 任一步失败整体回滚（chunk 原子语义）
	if _, err := tx.Exec(ctx, statCopyCreateSQL); err != nil {
		return err
	}
	if _, err := tx.CopyFrom(ctx, statCopyTable, statUpsertCols,
		pgx.CopyFromSlice(len(buckets), func(i int) ([]any, error) {
			return statCopyRow(buckets[i], now), nil
		})); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, statCopyMergeSQL); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// statCopyRow 单桶 → COPY 行（列序 = statUpsertCols；bucket_time/updated_at
// 单独取值，其余复用 statColValue）。
func statCopyRow(b *domain.StatBucket, now time.Time) []any {
	row := make([]any, 0, len(statUpsertCols))
	for _, col := range statUpsertCols {
		switch col {
		case "bucket_time":
			row = append(row, b.BucketTime)
		case "updated_at":
			row = append(row, now)
		default:
			row = append(row, statColValue(b, col))
		}
	}
	return row
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
	case "input_tokens":
		return b.InputTokens
	case "output_tokens":
		return b.OutputTokens
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
			InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			TotalTokens: row.TotalTokens, TotalLatencyMS: row.TotalLatencyMs,
			CacheReadTokens: row.CacheReadTokens, CacheCreationTokens: row.CacheCreationTokens,
			Cost: row.Cost,
		})
	}
	return out, nil
}
