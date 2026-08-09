package repository_test

// 统计批量 upsert 冲突路径真实 PG 测试（评审 I-2 升级 M，P0 修复）：DO
// UPDATE SET 曾把**维度列**也写成 old + excluded——model varchar+varchar →
// SQLSTATE 42883 整批失败（压测实证统计面零落库根因）、bigint 维度列相加
// 翻倍（ID 值失真）。同 bucket key 两次 Upsert 强制冲突 → 断言测量列累加、
// 维度列不翻倍、updated_at 更新。基座约定同 pg_account_groups_test：
// newPGRepos 每测试重建 schema（本包 PG 测试串行，无表级冲突）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent/usagestat"
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
