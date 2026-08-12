// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 统计批量 upsert 冲突路径真实 PG 测试（评审 I-2 升级 M，P0 修复）：DO
// UPDATE SET 曾把**维度列**也写成 old + excluded——model varchar+varchar →
// SQLSTATE 42883 整批失败（压测实证统计面零落库根因）、bigint 维度列相加
// 翻倍（ID 值失真）。同 bucket key 两次 Upsert 强制冲突 → 断言测量列累加、
// 维度列不翻倍、updated_at 更新。基座约定同 pg_account_groups_test：
// newPGRepos 每测试重建 schema（本包 PG 测试串行，无表级冲突）。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/usagestat"
)

func TestPGStatUpsertConflictAccumulates(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	bucket := time.Now().UTC().Truncate(time.Hour)
	mk := func(req, tok int64) *domain.StatBucket {
		return &domain.StatBucket{
			BucketTime: bucket, GroupID: 7, AccountID: 0, TemplateID: 0, UserID: 42,
			Model: "gpt-4o", IsError: false,
			RequestCount: req, ErrorCount: 0, InputTokens: 0, OutputTokens: 0,
			TotalTokens: tok, CacheReadTokens: 0, CacheCreationTokens: 0, Cost: 0, TotalLatencyMS: 0,
		}
	}
	require.NoError(t, repos.Stats.Upsert(ctx, []*domain.StatBucket{mk(3, 100)}))

	// 同 bucket key 二次 Upsert → 强制冲突：DO UPDATE SET 只对测量列加和
	//（修复前：维度列 model varchar+varchar → 42883 整批失败；group_id/
	// user_id bigint 相加翻倍）。
	first, err := repos.Client.UsageStat.Query().
		Where(usagestat.BucketTimeEQ(bucket), usagestat.GroupIDEQ(7), usagestat.UserIDEQ(42), usagestat.ModelEQ("gpt-4o")).
		Only(ctx)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond) // updated_at 更新断言的时间差
	require.NoError(t, repos.Stats.Upsert(ctx, []*domain.StatBucket{mk(2, 50)}))

	got, err := repos.Client.UsageStat.Query().
		Where(usagestat.BucketTimeEQ(bucket), usagestat.GroupIDEQ(7), usagestat.UserIDEQ(42), usagestat.ModelEQ("gpt-4o")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(5), got.RequestCount, "测量列冲突累加（3+2）")
	require.Equal(t, int64(150), got.TotalTokens, "测量列冲突累加（100+50）")
	require.Equal(t, int64(7), got.GroupID, "维度列不翻倍")
	require.Equal(t, int64(42), got.UserID, "维度列不翻倍")
	require.Equal(t, "gpt-4o", got.Model, "维度列不翻倍（修复前 42883）")
	require.False(t, got.IsError, "维度列不翻倍")
	require.Equal(t, int64(0), got.AccountID, "维度列不翻倍")
	require.Equal(t, int64(0), got.TemplateID, "维度列不翻倍")
	require.True(t, got.UpdatedAt.After(first.UpdatedAt), "updated_at 随冲突更新")
}

// TestPGStatUpsertCopyBulk COPY 两阶段批量路径（#17）：一事务内 COPY 多桶 →
// 单条 INSERT..SELECT..ON CONFLICT 合并。第二批含与存量冲突的行（DO UPDATE
// 累加）与新 key 行（INSERT）——同一条合并 SQL 双臂同走；维度列不翻倍。
// 注：同批次内不允许重复 key（PG 21000 "cannot affect row a second time"，
// 旧 raw VALUES 路径同错）——生产 flushStats 按 bucket key 去重分片，块内恒
// 无重复 key。
func TestPGStatUpsertCopyBulk(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Hour)
	mk := func(group, req, tok int64) *domain.StatBucket {
		return &domain.StatBucket{
			BucketTime: base, GroupID: group, AccountID: 0, TemplateID: 0, UserID: 42,
			Model: "gpt-4o", IsError: false,
			RequestCount: req, ErrorCount: 0, InputTokens: 0, OutputTokens: 0,
			TotalTokens: tok, CacheReadTokens: 0, CacheCreationTokens: 0, Cost: 0, TotalLatencyMS: 0,
		}
	}
	// 第一批：100 个不同 group 桶（全 INSERT）
	buckets := make([]*domain.StatBucket, 0, 100)
	for g := int64(1); g <= 100; g++ {
		buckets = append(buckets, mk(g, 1, 10*g))
	}
	require.NoError(t, repos.Stats.Upsert(ctx, buckets))

	// 第二批：group 1..50 与存量冲突（测量列累加）+ group 101..150 新 key（INSERT）
	buckets2 := make([]*domain.StatBucket, 0, 100)
	for g := int64(1); g <= 50; g++ {
		buckets2 = append(buckets2, mk(g, 2, 20))
	}
	for g := int64(101); g <= 150; g++ {
		buckets2 = append(buckets2, mk(g, 1, 7))
	}
	require.NoError(t, repos.Stats.Upsert(ctx, buckets2))

	total, err := repos.Client.UsageStat.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 150, total, "100 + 50 冲突累加 + 50 新 key = 150 行，不重复计")

	g1, err := repos.Client.UsageStat.Query().
		Where(usagestat.BucketTimeEQ(base), usagestat.GroupIDEQ(1), usagestat.UserIDEQ(42), usagestat.ModelEQ("gpt-4o")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), g1.RequestCount, "冲突累加（1+2）")
	require.Equal(t, int64(30), g1.TotalTokens, "冲突累加（10+20）")
	require.Equal(t, int64(1), g1.GroupID, "维度列不翻倍")
	require.Equal(t, "gpt-4o", g1.Model, "维度列不翻倍")

	g50, err := repos.Client.UsageStat.Query().
		Where(usagestat.BucketTimeEQ(base), usagestat.GroupIDEQ(50), usagestat.UserIDEQ(42), usagestat.ModelEQ("gpt-4o")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), g50.RequestCount, "冲突累加（1+2）")
	require.Equal(t, int64(520), g50.TotalTokens, "冲突累加（500+20）")

	g101, err := repos.Client.UsageStat.Query().
		Where(usagestat.BucketTimeEQ(base), usagestat.GroupIDEQ(101), usagestat.UserIDEQ(42), usagestat.ModelEQ("gpt-4o")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), g101.RequestCount, "新 key 原样落库")
	require.Equal(t, int64(7), g101.TotalTokens)
}

// #37 P3：多实例并发 Upsert 同批 bucket（模拟实例 A/B 各自 counters map 随机
// 迭代序 → 批量行序相反，锁顺序交错）——修复前 INSERT..SELECT..ON CONFLICT
// DO UPDATE 目标行锁顺序交错 → deadlock detected（40P01，压测实证偶发；本
// 测试 500 桶 × 4 并发已回退验证复现原报错）；修复后批内按冲突键排序（锁顺序
// 一致化）+ 40P01 瞬时重试兜底 → 并发无失败。断言全部成功 + 测量列冲突累加
// 精确（4 并发各计 1 → 4，维度列不翻倍）。
func TestPGStatUpsertConcurrentNoDeadlock(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Hour)
	mk := func(g int64) *domain.StatBucket {
		return &domain.StatBucket{
			BucketTime: base, GroupID: g, AccountID: 0, TemplateID: 0, UserID: 42,
			Model: "gpt-4o", IsError: false,
			RequestCount: 1, ErrorCount: 0, InputTokens: 0, OutputTokens: 0,
			TotalTokens: 10, CacheReadTokens: 0, CacheCreationTokens: 0, Cost: 0, TotalLatencyMS: 0,
		}
	}
	const n = 500 // 生产 flush 分块同量级（锁重叠窗口足够大，无修复必死锁）
	buckets := make([]*domain.StatBucket, 0, n)
	for g := int64(1); g <= n; g++ {
		buckets = append(buckets, mk(g))
	}
	require.NoError(t, repos.Stats.Upsert(ctx, buckets), "预置存量行（并发 DO UPDATE 冲突路径）")
	rev := make([]*domain.StatBucket, len(buckets)) // 实例 B：行序相反（map 随机迭代极端形态）
	for i := range buckets {
		rev[len(buckets)-1-i] = buckets[i]
	}

	start := make(chan struct{}) // 起跑屏障：最大化两批 merge 锁获取重叠
	var wg sync.WaitGroup
	errs := make([]error, 4) // 2 正序 + 2 倒序（多 worker 多实例形态）
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			b := buckets
			if i%2 == 1 {
				b = rev
			}
			b2 := make([]*domain.StatBucket, len(b)) // 每 goroutine 独立副本：避免并发排序同一数组（评审 M-1）
			copy(b2, b)
			errs[i] = repos.Stats.Upsert(ctx, b2)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		require.NoError(t, e, "并发 Upsert 同批 bucket 无 deadlock（排序 + 重试兜底）", i)
	}

	total, err := repos.Client.UsageStat.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, n, total, "500 桶不重复计（DO UPDATE 累加非新增）")
	g1, err := repos.Client.UsageStat.Query().
		Where(usagestat.BucketTimeEQ(base), usagestat.GroupIDEQ(1), usagestat.UserIDEQ(42), usagestat.ModelEQ("gpt-4o")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(5), g1.RequestCount, "预置 1 + 4 并发增量冲突累加精确")
	require.Equal(t, int64(50), g1.TotalTokens, "10 × 5 累加精确")
	require.Equal(t, int64(1), g1.GroupID, "维度列不翻倍")
	g250, err := repos.Client.UsageStat.Query().
		Where(usagestat.BucketTimeEQ(base), usagestat.GroupIDEQ(250), usagestat.UserIDEQ(42), usagestat.ModelEQ("gpt-4o")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(5), g250.RequestCount, "中间桶同样精确（锁顺序一致化的覆盖面）")
	g500, err := repos.Client.UsageStat.Query().
		Where(usagestat.BucketTimeEQ(base), usagestat.GroupIDEQ(500), usagestat.UserIDEQ(42), usagestat.ModelEQ("gpt-4o")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(5), g500.RequestCount, "末端桶同样精确")
}
