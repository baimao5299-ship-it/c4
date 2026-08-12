// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// Task C 落账面（spec §4.2/§4.3）真实 PG 测试：usage_logs 图片 6 列（image
// input/output tokens + image_count + 3 价格快照）5 处同步点 + format=openai-
// images 枚举扩展。覆盖：
//   - bootstrap 建表列存在断言（DROP 重建语义——usageLogColumnDefs 即终态，
//     无补列逻辑/无迁移；普通表路径 bootstrap 重建后同样含 6 列）
//   - ent CreateBulk 路径（InsertBatch）roundtrip：6 列有值 + format=openai-
//     images 落库、QueryUsages 读回、SQL 层直查
//   - 价格列 NULL 语义（未设置 → NULL 落库、nil 读回；token/count 列 DEFAULT 0）
//   - pgx COPY 路径（DeductAndLog + pool）：6 列 + openai-images 落库（COPY
//     逐行 FormatValidator 校验通过断言——不扩展则图片行 COPY 恒失败回灌）
//
// 基座约定同 pg_logcols_test.go：newPGRepos 每测试 DROP SCHEMA 重建 + migrate
//（钩子跳过 usagelog）+ 分区 bootstrap。

import (
	"context"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/repository"
)

// pgImageLogCols 图片 6 列元数据（data_type/is_nullable 断言用）。token/count
// 列 NOT NULL DEFAULT 0（与既有 input/output/total_tokens 同形态）；价格快照
// 列 NULL（nil = 无该分量价，对齐 pricings nil 语义）。
var pgImageLogCols = []struct {
	name       string
	dataType   string
	isNullable string
	columnDflt string
}{
	{"image_input_tokens", "bigint", "NO", "0"},
	{"image_output_tokens", "bigint", "NO", "0"},
	{"image_count", "bigint", "NO", "0"},
	{"price_image_input_millis", "bigint", "YES", ""},
	{"price_image_output_millis", "bigint", "YES", ""},
	{"price_per_image_millis", "bigint", "YES", ""},
}

func pgImageColMeta(t *testing.T, pool *pgxpool.Pool, name string) (dataType, isNullable, columnDflt string) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT data_type, is_nullable, COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'usage_logs' AND column_name = $1`, name).
		Scan(&dataType, &isNullable, &columnDflt)
	require.NoError(t, err, "bootstrap 建表必须含 %s 列", name)
	return
}

// TestUsageLogImageColumnsExistPG bootstrap 建表列存在断言：分区表 + 普通表
// DROP 重建两路径建表后 6 列齐全（类型/可空/默认值逐项断言）。
func TestUsageLogImageColumnsExistPG(t *testing.T) {
	newPGRepos(t) // bootstrap 副作用（分区表路径建表）
	pool := pgTestPool(t)

	// 分区表路径（newPGRepos 已 bootstrap）
	for _, c := range pgImageLogCols {
		dt, nul, dflt := pgImageColMeta(t, pool, c.name)
		require.Equal(t, c.dataType, dt, "%s 类型", c.name)
		require.Equal(t, c.isNullable, nul, "%s 可空", c.name)
		if c.columnDflt != "" {
			require.Contains(t, dflt, c.columnDflt, "%s 默认值", c.name)
		}
	}

	// 普通表 → bootstrap DROP 重建路径（用户裁决：直接重建即终态，无补列逻辑）
	ctx := context.Background()
	db := pgTestDB(t)
	_, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`)
	require.NoError(t, err)
	drv := entsql.OpenDB(dialect.Postgres, db)
	oldClient := ent.NewClient(ent.Driver(drv))
	require.NoError(t, oldClient.Schema.Create(ctx)) // 无钩子 migrate → 普通表（无图片列）
	repos2, err := repository.New(drv, true)
	require.NoError(t, err)
	require.NoError(t, repos2.EnsureUsageLogPartitioned(ctx, time.Now()))
	for _, c := range pgImageLogCols {
		dt, nul, _ := pgImageColMeta(t, pool, c.name)
		require.Equal(t, c.dataType, dt, "DROP 重建后 %s 类型", c.name)
		require.Equal(t, c.isNullable, nul, "DROP 重建后 %s 可空", c.name)
	}
	// 重建后插入含 6 列正常路由（无迁移终态语义）
	require.NoError(t, repos2.Usages.InsertBatch(ctx, []*domain.UsageLog{imageLogFor(0, "img-rebuilt")}))
	rows, err := repos2.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(2), rows[0].ImageCount, "重建后图片列落库读回")
}

// imageLogFor 图片计费日志（format=openai-images + 6 列有值，spec 实参形态：
// gpt-image-2 官方价 800,000/3,000,000 毫分 1M + aiml 5,400 毫分/张）。
func imageLogFor(userID int64, requestID string) *domain.UsageLog {
	l := logFor(userID, requestID)
	l.Format = domain.FormatOpenAIImages
	l.Model = "gpt-image-2"
	l.ImageInputTokens = 5000
	l.ImageOutputTokens = 20000
	l.ImageCount = 2
	l.PriceImageInputMillis = int64Ptr(800_000)
	l.PriceImageOutputMillis = int64Ptr(3_000_000)
	l.PricePerImageMillis = int64Ptr(5_400) // 毫分/张（例外单位）
	return l
}

// TestUsageLogImageColumnsRoundtripPG ent CreateBulk 路径（InsertBatch）：
// format=openai-images + 6 列有值落库 → QueryUsages 读回 + SQL 层直查；
// 未设置路径 → token/count 0（DEFAULT）、价格列 NULL/nil。
func TestUsageLogImageColumnsRoundtripPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		imageLogFor(0, "img-1"),
	}))
	rows, err := repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	got := rows[0]
	require.Equal(t, domain.FormatOpenAIImages, got.Format, "format=openai-images 落库读回")
	require.Equal(t, int64(5000), got.ImageInputTokens)
	require.Equal(t, int64(20000), got.ImageOutputTokens)
	require.Equal(t, int64(2), got.ImageCount)
	require.Equal(t, int64(800_000), *got.PriceImageInputMillis)
	require.Equal(t, int64(3_000_000), *got.PriceImageOutputMillis)
	require.Equal(t, int64(5_400), *got.PricePerImageMillis)

	// SQL 层直查（不经 domain 映射）
	var it, ot, cnt int64
	var pin, pout, per *int64
	err = pool.QueryRow(ctx, `SELECT image_input_tokens, image_output_tokens, image_count,
		price_image_input_millis, price_image_output_millis, price_per_image_millis
		FROM usage_logs WHERE request_id = 'img-1'`).
		Scan(&it, &ot, &cnt, &pin, &pout, &per)
	require.NoError(t, err)
	require.Equal(t, int64(5000), it)
	require.Equal(t, int64(20000), ot)
	require.Equal(t, int64(2), cnt)
	require.Equal(t, int64(800_000), *pin)
	require.Equal(t, int64(3_000_000), *pout)
	require.Equal(t, int64(5_400), *per)

	// 未设置路径：token/count 落 DEFAULT 0、价格列 NULL、读回 nil
	l2 := usageLogFor("img-2", time.Now().UTC()) // chat 普通日志，不设图片列
	l2.Format = domain.FormatOpenAIImages
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{l2}))
	rows, err = repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, r := range rows {
		if r.RequestID != "img-2" {
			continue
		}
		require.Zero(t, r.ImageInputTokens, "未设置 → 0（DEFAULT）")
		require.Zero(t, r.ImageOutputTokens)
		require.Zero(t, r.ImageCount)
		require.Nil(t, r.PriceImageInputMillis, "未设置价格 → nil")
		require.Nil(t, r.PriceImageOutputMillis)
		require.Nil(t, r.PricePerImageMillis)
	}
	for _, c := range []string{"price_image_input_millis", "price_image_output_millis", "price_per_image_millis"} {
		var raw *int64
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT `+c+` FROM usage_logs WHERE request_id = 'img-2'`).Scan(&raw))
		require.Nil(t, raw, "DB 层 %s 为 NULL", c)
	}
}

// TestUsageLogImageColumnsCopyPG pgx COPY 路径（DeductAndLog + pool）：6 列 +
// format=openai-images 落库——pgx InsertLogs 逐行 FormatValidator 校验通过
// （不扩展枚举则图片行 COPY 恒失败回灌，spec D4 评审实证路径）；SQL 层直查。
func TestUsageLogImageColumnsCopyPG(t *testing.T) {
	repos := newPGRepos(t) // pool != nil → pgx COPY 路径
	ctx := context.Background()
	pool := pgTestPool(t)
	u := seedPGUser(t, repos, "imgcopy@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))

	l := imageLogFor(u.ID, "img-copy-1")
	od, bal, err := repos.DeductAndLog(ctx, u.ID, 130, []*domain.UsageLog{l})
	require.NoError(t, err, "COPY 路径 openai-images 落库必须成功（FormatValidator 校验通过）")
	require.False(t, od)
	require.Equal(t, int64(999_870), bal)

	var it, ot, cnt int64
	var pin, pout, per *int64
	var format string
	err = pool.QueryRow(ctx, `SELECT format, image_input_tokens, image_output_tokens, image_count,
		price_image_input_millis, price_image_output_millis, price_per_image_millis
		FROM usage_logs WHERE request_id = 'img-copy-1'`).
		Scan(&format, &it, &ot, &cnt, &pin, &pout, &per)
	require.NoError(t, err)
	require.Equal(t, "openai-images", format)
	require.Equal(t, int64(5000), it)
	require.Equal(t, int64(20000), ot)
	require.Equal(t, int64(2), cnt)
	require.Equal(t, int64(800_000), *pin)
	require.Equal(t, int64(3_000_000), *pout)
	require.Equal(t, int64(5_400), *per)
}

// TestUsageLogImageFormatValidator 客户端面校验（spec §4.3）：ent 生成的
// FormatValidator 接受 openai-images、拒绝未知值——两条插入路径都走该校验
// （CreateBulk Save 前 / COPY 逐行前置）。
func TestUsageLogImageFormatValidator(t *testing.T) {
	require.NoError(t, usagelog.FormatValidator(usagelog.FormatOpenaiImages),
		"ent FormatValidator 必须接受 openai-images（否则 CreateBulk/COPY 双双拒绝）")
	require.Error(t, usagelog.FormatValidator(usagelog.Format("bogus-image-format")),
		"未知 format 拒绝（非法枚举语义保持）")
	require.True(t, domain.FormatOpenAIImages.Valid())
}
