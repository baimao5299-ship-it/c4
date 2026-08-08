package repository_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// 真实 PG 基座（newPGRepos；TEST_DATABASE_URL 未设置 → Skip）：
// 模型价格 Task 1 全部测试 —— 优先级闭环（manual > litellm）、DeleteManual
// 语义、UpsertFromLiteLLM 分批/部分成功（评审 M-2）、ListPricing 筛选/分页/sort。

func int64Ptr(v int64) *int64 { return &v }

// litellmRow 构造拉取源价格行。
func litellmRow(model string, prompt, completion int64) *domain.Pricing {
	return &domain.Pricing{
		Model: model, PromptPricePerMillion: prompt,
		CompletionPricePerMillion: completion, Source: domain.PricingSourceLitellm,
	}
}

// TestPricingPriorityPG 优先级闭环（行级互斥 manual > litellm）：
// 1) 先手动后拉取 → 价格不变（DO UPDATE 被 WHERE source != 'manual' 过滤）；
// 2) 先拉取后手动 → 接管（source=manual 价格变）；
// 3) 删手动行后拉取 → 恢复拉取价。
func TestPricingPriorityPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("litellm sync does not overwrite manual price", func(t *testing.T) {
		m := "gpm-pri-manual-a"
		p, err := repos.UpsertManual(ctx, m, 100, 200)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source)
		require.Equal(t, m, p.Model)

		// 拉取同 model → 手动价不变，且被过滤的行不计入更新数
		n, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{litellmRow(m, 999, 999)})
		require.NoError(t, err)
		require.Zero(t, n, "manual 行被 WHERE 过滤，不产生更新")

		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(100), got.PromptPricePerMillion, "手动价不被覆盖")
		require.Equal(t, int64(200), got.CompletionPricePerMillion)
		require.Equal(t, domain.PricingSourceManual, got.Source)
	})

	t.Run("manual upsert takes over litellm row", func(t *testing.T) {
		m := "gpm-pri-litellm-b"
		n, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{litellmRow(m, 10, 20)})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceLitellm, got.Source)

		p, err := repos.UpsertManual(ctx, m, 300, 400)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source)
		require.Equal(t, int64(300), p.PromptPricePerMillion, "litellm 行被接管")
		require.Equal(t, m, p.Model, "返回行 model 一致")

		got, err = repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, got.Source, "source 改为 manual")
		require.Equal(t, int64(300), got.PromptPricePerMillion)
		require.Equal(t, int64(400), got.CompletionPricePerMillion)
	})

	t.Run("delete manual restores litellm price on next sync", func(t *testing.T) {
		m := "gpm-pri-restore-c"
		_, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{litellmRow(m, 10, 20)})
		require.NoError(t, err)
		_, err = repos.UpsertManual(ctx, m, 500, 600)
		require.NoError(t, err)
		require.NoError(t, repos.DeleteManual(ctx, m), "manual 行可删")

		_, err = repos.GetPricing(ctx, m)
		require.ErrorIs(t, err, repository.ErrNotFound, "删手动行后整行消失（下轮拉取补回）")

		n, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{litellmRow(m, 10, 20)})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceLitellm, got.Source)
		require.Equal(t, int64(10), got.PromptPricePerMillion, "恢复拉取价")
	})
}

// TestPricingDeleteManualPG DeleteManual 语义：litellm 行 → ErrConflict
// （只允许删手动价，防误删拉取行）；不存在 → ErrNotFound；manual 行成功删除。
func TestPricingDeleteManualPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("litellm row is not deletable", func(t *testing.T) {
		m := "gpm-del-litellm"
		_, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{litellmRow(m, 1, 2)})
		require.NoError(t, err)
		err = repos.DeleteManual(ctx, m)
		require.ErrorIs(t, err, repository.ErrConflict, "litellm 行 → ErrConflict")
		require.Contains(t, err.Error(), `model="gpm-del-litellm"`)
		_, err = repos.GetPricing(ctx, m)
		require.NoError(t, err, "litellm 行保留未被删除")
	})

	t.Run("missing model is not found", func(t *testing.T) {
		err := repos.DeleteManual(ctx, "gpm-no-such-model")
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.Contains(t, err.Error(), `model="gpm-no-such-model"`)
	})

	t.Run("manual row deleted", func(t *testing.T) {
		m := "gpm-del-manual"
		_, err := repos.UpsertManual(ctx, m, 1, 2)
		require.NoError(t, err)
		require.NoError(t, repos.DeleteManual(ctx, m))
		_, err = repos.GetPricing(ctx, m)
		require.ErrorIs(t, err, repository.ErrNotFound, "manual 行已删除")
	})
}

// TestPricingUpsertLitellmBatchPG 分批（评审 M-2）：
// 1) 1200 行（3 批）全成功，成功数 = 总行数，max_tokens 空/非空 roundtrip；
// 2) 批内含手动行 → WHERE 过滤不覆盖，成功数相应减少；
// 3) 部分成功：批 0 内同 model 重复（同批互冲突 → 整批失败）→ 其余批成功，
//    返回成功条数 + 首个错误。
func TestPricingUpsertLitellmBatchPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	// 1) >500 行全成功
	const total = 1200
	rows := make([]*domain.Pricing, 0, total)
	for i := 0; i < total; i++ {
		p := litellmRow(fmt.Sprintf("litellm-batch-%04d", i), int64(i), int64(i*2))
		if i%2 == 0 {
			p.MaxInputTokens = int64Ptr(int64(1000 + i))
			p.MaxOutputTokens = int64Ptr(int64(2000 + i))
		}
		rows = append(rows, p)
	}
	n, err := repos.UpsertFromLiteLLM(ctx, rows)
	require.NoError(t, err)
	require.Equal(t, total, n, "全部插入成功")

	got, err := repos.GetPricing(ctx, "litellm-batch-0000")
	require.NoError(t, err)
	require.Equal(t, int64(0), got.PromptPricePerMillion)
	require.Equal(t, int64(0), got.CompletionPricePerMillion)
	require.NotNil(t, got.MaxInputTokens, "偶行 max_input_tokens roundtrip")
	require.Equal(t, int64(1000), *got.MaxInputTokens)
	require.Equal(t, int64(2000), *got.MaxOutputTokens)
	require.Equal(t, domain.PricingSourceLitellm, got.Source)

	got, err = repos.GetPricing(ctx, "litellm-batch-0001")
	require.NoError(t, err)
	require.Nil(t, got.MaxInputTokens, "奇行 nil max_tokens roundtrip")
	require.Equal(t, int64(2), got.CompletionPricePerMillion)

	// 2) 批内含手动行：DO UPDATE 被 WHERE 过滤，成功数 = 总行数 - 1
	manualModel := "litellm-batch-0600"
	_, err = repos.UpsertManual(ctx, manualModel, 777, 888)
	require.NoError(t, err)
	n, err = repos.UpsertFromLiteLLM(ctx, rows)
	require.NoError(t, err)
	require.Equal(t, total-1, n, "manual 行被过滤不计入更新数")
	got, err = repos.GetPricing(ctx, manualModel)
	require.NoError(t, err)
	require.Equal(t, int64(777), got.PromptPricePerMillion, "手动价不被覆盖")
	require.Equal(t, domain.PricingSourceManual, got.Source)

	// 3) 部分成功：批 0（500 行）内含同 model 两行 → 整批失败，批 1 成功
	dupRows := make([]*domain.Pricing, 0, 502)
	for i := 0; i < 502; i++ {
		m := fmt.Sprintf("litellm-dup-%04d", i)
		if i == 250 || i == 251 {
			m = "litellm-dup-0250" // 同批内重复 → PG 报 "cannot affect row a second time"
		}
		dupRows = append(dupRows, litellmRow(m, int64(i), 0))
	}
	n, err = repos.UpsertFromLiteLLM(ctx, dupRows)
	require.Error(t, err, "批 0 失败返回错误（部分成功可接受）")
	require.Equal(t, 2, n, "仅批 1（2 行）成功")
	got, err = repos.GetPricing(ctx, "litellm-dup-0500")
	require.NoError(t, err, "批 1 行已落库")
	require.Equal(t, int64(500), got.PromptPricePerMillion)
	_, err = repos.GetPricing(ctx, "litellm-dup-0250")
	require.ErrorIs(t, err, repository.ErrNotFound, "批 0 整体失败无残留")
}

// TestPricingListPG 列表：全量分页 / source 筛选 / model 模糊（不区分大小写）/
// sort 白名单（model/updated_at 合法，非法 → ErrInvalidSort）。
func TestPricingListPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	_, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{
		litellmRow("gpt-4o", 1, 2),
		litellmRow("claude-sonnet", 3, 4),
	})
	require.NoError(t, err)
	_, err = repos.UpsertManual(ctx, "gpt-4o-mini", 5, 6)
	require.NoError(t, err)
	_, err = repos.UpsertManual(ctx, "gemini-pro", 7, 8)
	require.NoError(t, err)

	// 全量（默认分页 id desc）
	rows, total, err := repos.ListPricing(ctx, repository.ListQuery{Limit: 10}, nil, "")
	require.NoError(t, err)
	require.Equal(t, int64(4), total)
	require.Len(t, rows, 4)

	// 筛选 source=manual
	ms := domain.PricingSourceManual
	rows, total, err = repos.ListPricing(ctx, repository.ListQuery{}, &ms, "")
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	for _, p := range rows {
		require.Equal(t, domain.PricingSourceManual, p.Source)
	}

	// model 模糊（ilike，不区分大小写）：gpt → 2 行
	_, total, err = repos.ListPricing(ctx, repository.ListQuery{}, nil, "gpt")
	require.NoError(t, err)
	require.Equal(t, int64(2), total)

	// 分页 + sort model asc（全小写首字母，排序与库 collation 无关）
	rows, total, err = repos.ListPricing(ctx, repository.ListQuery{Limit: 2, Offset: 0, Sort: "model", Order: "asc"}, nil, "")
	require.NoError(t, err)
	require.Equal(t, int64(4), total)
	require.Len(t, rows, 2)
	require.Equal(t, "claude-sonnet", rows[0].Model)
	require.Equal(t, "gemini-pro", rows[1].Model)

	// sort updated_at desc 合法
	rows, total, err = repos.ListPricing(ctx, repository.ListQuery{Limit: 10, Sort: "updated_at", Order: "desc"}, nil, "")
	require.NoError(t, err)
	require.Equal(t, int64(4), total)
	require.Len(t, rows, 4)

	// 非法 sort → ErrInvalidSort
	_, _, err = repos.ListPricing(ctx, repository.ListQuery{Sort: "bogus"}, nil, "")
	require.ErrorIs(t, err, repository.ErrInvalidSort)
}
