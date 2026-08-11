package repository_test

// usage_stats 分区化（用户裁决 2026-08-11：PG DELETE 不释放空间，180 天保留
// 清理必须分区 DROP O(1)——替代逐行 DELETE 方案）真实 PG 测试：bootstrap 幂等
// / 分区键（bucket_time）upsert 路由 + 冲突累加 / 180 天 cutoff DROP 边界 /
// 并发 bootstrap 幂等。
//
// 基座约定同 pg_partition_test.go：newPGRepos 每测试 DROP SCHEMA 重建 + migrate
//（钩子跳过三表）+ 分区 bootstrap（EnsureUsageLogPartitioned +
// EnsureErrLogPartitioned + EnsureUsageStatsPartitioned）。本包 PG 测试串行
//（无 t.Parallel），无表级冲突。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// statBucketFor 构造小时桶（bucket_time 为分区键；维度列非零——唯一索引键）。
func statBucketFor(at time.Time, req int64) *domain.StatBucket {
	return &domain.StatBucket{
		BucketTime: at, GroupID: 3, AccountID: 0, TemplateID: 0, UserID: 9,
		Model: "gpt-4o", IsError: false,
		RequestCount: req, ErrorCount: 0, InputTokens: 0, OutputTokens: 0,
		TotalTokens: 10 * req, CacheReadTokens: 0, CacheCreationTokens: 0,
		Cost: 0, TotalLatencyMS: 0,
	}
}

// pgUsageStatsPartitionNames 当前 usage_stats 分区名列表（pgPartitionNames
// 是 usage_logs 专用，各自表名参数化——同构 SQL）。
func pgUsageStatsPartitionNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT c.relname FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
		 JOIN pg_class p ON p.oid = i.inhparent JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE p.relname = 'usage_stats' AND n.nspname = current_schema()`)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	return names
}

// TestUsageStatsPartitionBootstrapPG usage_stats bootstrap 幂等 + 分区表结构：
// 二次 bootstrap 不重建（数据保留），预建当日/明日分区（bucket_time 日界）+
// 唯一索引（ON CONFLICT 目标，含分区键）。
func TestUsageStatsPartitionBootstrapPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	parted, err := repos.Partitions.IsUsageStatsPartitioned(ctx)
	require.NoError(t, err)
	require.True(t, parted, "migrate 后 bootstrap 必须建 usage_stats 分区表")

	// 数据保留验证幂等：upsert 一桶 → 二次 bootstrap → 桶仍在、仍分区
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)
	require.NoError(t, repos.Stats.Upsert(ctx, []*domain.StatBucket{statBucketFor(bucket, 1)}))
	require.NoError(t, repos.EnsureUsageStatsPartitioned(ctx, time.Now()))
	parted, err = repos.Partitions.IsUsageStatsPartitioned(ctx)
	require.NoError(t, err)
	require.True(t, parted)
	got, err := repos.Stats.ScanStats(ctx, repository.StatQuery{From: bucket, To: bucket.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, got, 1, "二次 bootstrap 不重建（数据保留）")
	require.Equal(t, int64(1), got[0].RequestCount)

	// 预建分区：当日 + 明日（bucket_time 日界）；唯一索引 + 时间索引齐
	day := bucket.Truncate(24 * time.Hour)
	require.Contains(t, pgUsageStatsPartitionNames(t, pool), "usage_stats_"+day.Format("20060102"))
	require.Contains(t, pgUsageStatsPartitionNames(t, pool), "usage_stats_"+day.AddDate(0, 0, 1).Format("20060102"))
	var n int64
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'usage_stats' AND indexname IN ('usagestat_bucket_time','usagestat_bucket_time_group_id_account_id_template_id_user_id_model_is_error')`).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, int64(2), n, "bootstrap 建齐 唯一索引（ON CONFLICT 目标）+ 时间索引")
}

// TestUsageStatsPartitionUpsertRoutingPG 分区键 upsert 正确（用户裁决要求）：
// 跨日两桶 Upsert → 按 bucket_time 路由到各自日分区；同分区同 key 二次 Upsert
// → ON CONFLICT DO UPDATE 累加（分区表上行为不变，单行不重复）。
func TestUsageStatsPartitionUpsertRoutingPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	now := time.Now().UTC()
	dayA := now.Truncate(24 * time.Hour)
	dayB := dayA.AddDate(0, 0, 1)
	require.NoError(t, repos.EnsureUsageStatsPartitions(ctx, dayB, dayB)) // 明日分区已由 bootstrap 预建，幂等兜底

	// 跨日两桶（分区键 = bucket_time：A 区 1 桶 + B 区 1 桶）
	bucketA := dayA.Add(10 * time.Hour)
	bucketB := dayB.Add(2 * time.Hour)
	require.NoError(t, repos.Stats.Upsert(ctx, []*domain.StatBucket{
		statBucketFor(bucketA, 1),
		statBucketFor(bucketB, 1),
	}))

	// 路由正确：各自日分区恰好一行
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_stats_`+dayA.Format("20060102")))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_stats_`+dayB.Format("20060102")))

	// 同分区同 key 二次 Upsert → 冲突累加（测量列），不新增行
	require.NoError(t, repos.Stats.Upsert(ctx, []*domain.StatBucket{statBucketFor(bucketA, 2)}))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_stats_`+dayA.Format("20060102")), "分区表上冲突累加不重复计")
	var req int64
	err := pool.QueryRow(ctx, `SELECT request_count FROM usage_stats_`+dayA.Format("20060102")+` WHERE group_id = 3 AND user_id = 9`).Scan(&req)
	require.NoError(t, err)
	require.Equal(t, int64(3), req, "测量列冲突累加（1+2）——分区表上 upsert 行为不变")
	// 另一日分区不受影响
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_stats_`+dayB.Format("20060102")))
}

// TestUsageStatsPartitionRetentionPG 180 天保留期 DROP 边界（用户裁决核心）：
// cutoff = now - 180 天 → 181 天前分区 DROP O(1)，当日/近期分区保留。
func TestUsageStatsPartitionRetentionPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	now := time.Now().UTC()
	// 构造 181 天前的历史分区 + 桶（预建历史分区，幂等）
	old := now.Truncate(24*time.Hour).AddDate(0, 0, -181)
	require.NoError(t, repos.EnsureUsageStatsPartitions(ctx, old, old))
	oldBucket := old.Add(12 * time.Hour)
	require.NoError(t, repos.Stats.Upsert(ctx, []*domain.StatBucket{statBucketFor(oldBucket, 1)}))

	// 180 天保留：cutoff = now-180d → 181 天前分区该删（DROP O(1)）
	n, err := repos.DropUsageStatsPartitionsBefore(ctx, now.AddDate(0, 0, -180))
	require.NoError(t, err)
	require.Equal(t, 1, n, "181 天前分区该删（180 天保留期）")
	require.Zero(t, pgCount(t, pool, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = current_schema() AND c.relname = 'usage_stats_`+old.Format("20060102")+`'`),
		"usage_stats 历史分区已 DROP（替代 DELETE——不残留数据文件）")

	// 当日/明日分区保留
	day := now.Truncate(24 * time.Hour)
	require.Contains(t, pgUsageStatsPartitionNames(t, pool), "usage_stats_"+day.Format("20060102"))
	require.Contains(t, pgUsageStatsPartitionNames(t, pool), "usage_stats_"+day.AddDate(0, 0, 1).Format("20060102"))

	// 近期桶不受影响：预置当日桶 → DROP 后仍在
	require.NoError(t, repos.Stats.Upsert(ctx, []*domain.StatBucket{statBucketFor(day.Add(3*time.Hour), 5)}))
	n, err = repos.DropUsageStatsPartitionsBefore(ctx, now.AddDate(0, 0, -180))
	require.NoError(t, err)
	require.Zero(t, n, "无过期分区可删（幂等）")
	got, err := repos.Stats.ScanStats(ctx, repository.StatQuery{From: day.Add(3 * time.Hour), To: day.Add(4 * time.Hour)})
	require.NoError(t, err)
	require.Len(t, got, 1, "当日桶数据完整")
	require.Equal(t, int64(5), got[0].RequestCount)
}

// TestUsageStatsPartitionConcurrentBootstrapPG 并发 bootstrap 幂等（评审 I-1
// 多实例语义）：四实例同时 EnsureUsageStatsPartitioned——42P07/23505 容忍收敛，
// 无错误；数据保留。
func TestUsageStatsPartitionConcurrentBootstrapPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = repos.EnsureUsageStatsPartitioned(ctx, time.Now())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "并发 bootstrap 第 %d 个必须成功", i)
	}
	parted, err := repos.Partitions.IsUsageStatsPartitioned(ctx)
	require.NoError(t, err)
	require.True(t, parted)
	// 二次（串行）幂等：数据保留
	require.NoError(t, repos.Stats.Upsert(ctx, []*domain.StatBucket{statBucketFor(time.Now().UTC().Truncate(time.Hour), 1)}))
	require.NoError(t, repos.EnsureUsageStatsPartitioned(ctx, time.Now()))
}
