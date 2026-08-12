// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// imageBillLogFor 图片计费落账行（spec §4.2 六列：image_input/output_tokens +
// image_count + 价格快照三列；text 分量恒 0；TotalTokens 含 image tokens 不含
// 张数）。Cost 按 ImageCost 实参口径：100×800000/1e6 + 50×3000000/1e6 +
// 2×5400 = 11030。命名区别于 C 的 imageLogFor（pg_image_logcols_test.go——
// 本 helper 走 DeductAndLog 计费扣减断言，C 走 InsertBatch/QueryUsages）。
func imageBillLogFor(userID int64, requestID string) *domain.UsageLog {
	i64 := func(v int64) *int64 { return &v }
	l := logFor(userID, requestID)
	l.Format = domain.FormatOpenAIImages
	l.ImageInputTokens = 100
	l.ImageOutputTokens = 50
	l.ImageCount = 2
	l.PriceImageInputMillis = i64(800000)
	l.PriceImageOutputMillis = i64(3000000)
	l.PricePerImageMillis = i64(5400)
	l.InputTokens, l.OutputTokens = 0, 0
	l.TotalTokens = 150
	l.Cost = 11030
	return l
}

// TestPGUsageLogImageColumnsRoundTrip 图片计费落账 6 列往返（spec §4.2 同步点
// 2/3/4/5 全链）：COPY 路径 + ent CreateBulk 路径（DeductAndLog 双实现——
// NewWithPG pool → COPY、New → ent）写入 format=openai-images 行 → 分区表
// 落库（usageLogColumnDefs 单一事实源含列）→ QueryUsages 完整读回。任一同步
// 点漏加（COPY 列清单/行值/CreateBulk/列定义/查询映射）本测试即红。
func TestPGUsageLogImageColumnsRoundTrip(t *testing.T) {
	reposCopy := newPGRepos(t)      // pool → pgx COPY 路径
	reposEnt := newPGReposNoPool(t) // nil pool → ent CreateBulk 路径（同 schema）
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		repos *repository.Repository
	}{
		{"COPY 路径", reposCopy},
		{"ent CreateBulk 路径", reposEnt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := seedPGUser(t, tc.repos, "img-"+tc.name+"@example.com")
			require.NoError(t, tc.repos.UpdateUserBalance(ctx, u.ID, 1000000))
			l := imageBillLogFor(u.ID, "img-req-"+tc.name)
			od, bal, err := tc.repos.DeductAndLog(ctx, u.ID, l.Cost, []*domain.UsageLog{l})
			require.NoError(t, err)
			require.False(t, od)
			require.Equal(t, int64(1000000-l.Cost), bal, "按 image 分量 cost 扣减")

			rows, err := tc.repos.QueryUsages(ctx, repository.UsageQuery{UserID: u.ID, From: ptrTime(time.Now().Add(-time.Hour)), To: ptrTime(time.Now().Add(time.Hour))})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			got := rows[0]
			require.Equal(t, domain.FormatOpenAIImages, got.Format, "format=openai-images 落库")
			require.Equal(t, int64(2), got.ImageCount)
			require.Equal(t, int64(100), got.ImageInputTokens)
			require.Equal(t, int64(50), got.ImageOutputTokens)
			require.Zero(t, got.InputTokens, "text 分量恒 0")
			require.Zero(t, got.OutputTokens)
			require.Equal(t, int64(150), got.TotalTokens, "image tokens 入 TotalTokens")
			require.NotNil(t, got.PriceImageInputMillis)
			require.Equal(t, int64(800000), *got.PriceImageInputMillis)
			require.NotNil(t, got.PriceImageOutputMillis)
			require.Equal(t, int64(3000000), *got.PriceImageOutputMillis)
			require.NotNil(t, got.PricePerImageMillis)
			require.Equal(t, int64(5400), *got.PricePerImageMillis)
			require.Equal(t, int64(11030), got.Cost)
			require.Equal(t, l.RequestID, got.RequestID)
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
