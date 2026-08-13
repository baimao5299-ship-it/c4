// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 真实 PG 基座（newPGRepos；TEST_DATABASE_URL 未设置 → Skip）：
// function_price 价格表三件套全部测试——优先级闭环（manual > litellm，与
// pricings/image_price 同款）、codex-search 种子幂等、DeleteManual 语义、
// raw JSONB 落库、List 筛选/分页/sort、三表独立并存。

// functionLitellmRow 构造拉取源按单元价行（price_per_call 毫分/次）。
func functionLitellmRow(model string, perCall int64) *domain.FunctionPrice {
	return &domain.FunctionPrice{
		Model:        model,
		PricePerCall: int64Ptr(perCall),
		Source:       domain.PricingSourceLitellm,
	}
}

func functionManualReq(model string, perCall int64) *repository.FunctionPriceManual {
	return &repository.FunctionPriceManual{
		Model:        model,
		PricePerCall: int64Ptr(perCall),
	}
}

// TestFunctionPricePriorityPG 优先级闭环（行级互斥 manual > litellm，与
// pricings/image_price 同款机制）：
// 1) 先手动后拉取 → 价格不变（DO UPDATE 被 WHERE source != 'manual' 过滤）；
// 2) 先拉取后手动 → 接管（source=manual 价格变）；
// 3) 删手动行后拉取 → 恢复拉取价。
func TestFunctionPricePriorityPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("litellm sync does not overwrite manual price", func(t *testing.T) {
		m := "c3api-fn-pri-manual-a"
		p, err := repos.UpsertFunctionManual(ctx, functionManualReq(m, 42))
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source)
		require.Equal(t, int64(42), *p.PricePerCall)

		n, err := repos.UpsertFunctionFromLiteLLM(ctx, []*domain.FunctionPrice{functionLitellmRow(m, 5000)})
		require.NoError(t, err)
		require.Equal(t, 0, n, "manual 行被 WHERE 过滤，不更新")

		p, err = repos.GetFunctionPrice(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(42), *p.PricePerCall, "litellm 同步不覆盖手动价")
		require.Equal(t, domain.PricingSourceManual, p.Source)
	})

	t.Run("manual upsert takes over litellm row", func(t *testing.T) {
		m := "c3api-fn-pri-litellm-a"
		n, err := repos.UpsertFunctionFromLiteLLM(ctx, []*domain.FunctionPrice{functionLitellmRow(m, 5000)})
		require.NoError(t, err)
		require.Equal(t, 1, n)

		p, err := repos.UpsertFunctionManual(ctx, functionManualReq(m, 77))
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source)
		require.Equal(t, int64(77), *p.PricePerCall, "手动设价接管 litellm 行")
	})

	t.Run("delete manual then sync restores litellm price", func(t *testing.T) {
		m := "c3api-fn-pri-restore-a"
		_, err := repos.UpsertFunctionManual(ctx, functionManualReq(m, 42))
		require.NoError(t, err)
		require.NoError(t, repos.DeleteFunctionManual(ctx, m))

		_, err = repos.GetFunctionPrice(ctx, m)
		require.ErrorIs(t, err, repository.ErrNotFound, "删手动价后整行消失（缺失窗口）")

		n, err := repos.UpsertFunctionFromLiteLLM(ctx, []*domain.FunctionPrice{functionLitellmRow(m, 5000)})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		p, err := repos.GetFunctionPrice(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(5000), *p.PricePerCall, "下轮拉取补回 litellm 价")
		require.Equal(t, domain.PricingSourceLitellm, p.Source)
	})
}

// TestFunctionPriceDeleteManualPG DeleteManual 语义：litellm 行 → ErrConflict、
// 不存在 → ErrNotFound。
func TestFunctionPriceDeleteManualPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	_, err := repos.UpsertFunctionFromLiteLLM(ctx, []*domain.FunctionPrice{functionLitellmRow("c3api-fn-del-litellm", 5000)})
	require.NoError(t, err)
	err = repos.DeleteFunctionManual(ctx, "c3api-fn-del-litellm")
	require.ErrorIs(t, err, repository.ErrConflict, "litellm 行只允许删手动价")

	err = repos.DeleteFunctionManual(ctx, "c3api-fn-del-missing")
	require.ErrorIs(t, err, repository.ErrNotFound)
}

// TestFunctionPriceSeedPG codex-search 初始化种子幂等：首插 → 行存在（1000
// 毫分/次、manual）；重复调用不报错不覆盖；管理端改价后种子不覆盖改后值。
func TestFunctionPriceSeedPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	require.NoError(t, repos.EnsureFunctionPriceSeed(ctx))
	p, err := repos.GetFunctionPrice(ctx, domain.CodexSearchModel)
	require.NoError(t, err)
	require.Equal(t, domain.DefaultCodexSearchPricePerCall, *p.PricePerCall, "种子价 $0.01/次 = 1000 毫分")
	require.Equal(t, domain.PricingSourceManual, p.Source, "种子 source=manual")

	// 幂等：重复调用不报错、不覆盖
	require.NoError(t, repos.EnsureFunctionPriceSeed(ctx))
	p, err = repos.GetFunctionPrice(ctx, domain.CodexSearchModel)
	require.NoError(t, err)
	require.Equal(t, domain.DefaultCodexSearchPricePerCall, *p.PricePerCall)

	// 管理端改价后种子不覆盖（ON CONFLICT DO NOTHING）
	_, err = repos.UpsertFunctionManual(ctx, functionManualReq(domain.CodexSearchModel, 999))
	require.NoError(t, err)
	require.NoError(t, repos.EnsureFunctionPriceSeed(ctx))
	p, err = repos.GetFunctionPrice(ctx, domain.CodexSearchModel)
	require.NoError(t, err)
	require.Equal(t, int64(999), *p.PricePerCall, "种子不覆盖管理端改价")

	// 删除后种子幂等补回
	require.NoError(t, repos.DeleteFunctionManual(ctx, domain.CodexSearchModel))
	require.NoError(t, repos.EnsureFunctionPriceSeed(ctx))
	p, err = repos.GetFunctionPrice(ctx, domain.CodexSearchModel)
	require.NoError(t, err)
	require.Equal(t, domain.DefaultCodexSearchPricePerCall, *p.PricePerCall, "删除后重启 bootstrap 补回")
}

// TestFunctionPriceLitellmUpsertPG 拉取落库：price_per_call + raw JSONB 完整
// 镜像 + source=litellm；与 pricings/image_price 同条目并存互不干扰。
func TestFunctionPriceLitellmUpsertPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	raw := []byte(`{"mode":"search","input_cost_per_query":0.0001,"rpm":60}`)
	n, err := repos.UpsertFunctionFromLiteLLM(ctx, []*domain.FunctionPrice{{
		Model:        "c3api-fn-raw",
		PricePerCall: int64Ptr(10),
		Raw:          raw,
		Source:       domain.PricingSourceLitellm,
	}})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	p, err := repos.GetFunctionPrice(ctx, "c3api-fn-raw")
	require.NoError(t, err)
	require.Equal(t, int64(10), *p.PricePerCall)
	require.Equal(t, domain.PricingSourceLitellm, p.Source)
	require.NotNil(t, p.Raw, "raw 完整镜像落库")
	require.Contains(t, string(p.Raw), "input_cost_per_query", "raw 含未映射字段")
	require.Contains(t, string(p.Raw), "rpm")

	// 更新已存在的 litellm 行（非 manual）→ 价格与 raw 均更新
	n, err = repos.UpsertFunctionFromLiteLLM(ctx, []*domain.FunctionPrice{{
		Model:        "c3api-fn-raw",
		PricePerCall: int64Ptr(20),
		Raw:          []byte(`{"mode":"search","input_cost_per_query":0.0002}`),
		Source:       domain.PricingSourceLitellm,
	}})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	p, err = repos.GetFunctionPrice(ctx, "c3api-fn-raw")
	require.NoError(t, err)
	require.Equal(t, int64(20), *p.PricePerCall, "litellm 行更新")
	require.Contains(t, string(p.Raw), "0.0002")

	// 同条目三表独立并存：文本价 + image 价 + 按单元价各自落各自表
	_, err = repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{{Model: "c3api-fn-raw", PromptPricePerMillion: 1, CompletionPricePerMillion: 1, Source: domain.PricingSourceLitellm}})
	require.NoError(t, err)
	_, err = repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{{Model: "c3api-fn-raw", InputImageTokenPricePerMillion: int64Ptr(1), Source: domain.PricingSourceLitellm}})
	require.NoError(t, err)
	pr, err := repos.GetPricing(ctx, "c3api-fn-raw")
	require.NoError(t, err)
	require.Equal(t, int64(1), pr.PromptPricePerMillion)
	ip, err := repos.GetImagePrice(ctx, "c3api-fn-raw")
	require.NoError(t, err)
	require.NotNil(t, ip.InputImageTokenPricePerMillion)
	fp, err := repos.GetFunctionPrice(ctx, "c3api-fn-raw")
	require.NoError(t, err)
	require.Equal(t, int64(20), *fp.PricePerCall, "三表并存互不干扰")
}

// TestFunctionPriceListPG List 分页/筛选/sort 白名单：source/model 筛选、
// sort 非法 → ErrInvalidSort。
func TestFunctionPriceListPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	_, err := repos.UpsertFunctionFromLiteLLM(ctx, []*domain.FunctionPrice{
		functionLitellmRow("search-a", 10),
		functionLitellmRow("search-b", 20),
		functionLitellmRow("search-c", 30),
	})
	require.NoError(t, err)
	_, err = repos.UpsertFunctionManual(ctx, functionManualReq("codex-search", 1000))
	require.NoError(t, err)

	rows, total, err := repos.ListFunctionPrice(ctx, repository.ListQuery{Limit: 20, Sort: "model", Order: "asc"}, nil, "")
	require.NoError(t, err)
	require.Equal(t, int64(4), total)
	require.Len(t, rows, 4)
	require.Equal(t, "codex-search", rows[0].Model, "model asc 排序")

	litellm := domain.PricingSourceLitellm
	rows, total, err = repos.ListFunctionPrice(ctx, repository.ListQuery{Limit: 20}, &litellm, "")
	require.NoError(t, err)
	require.Equal(t, int64(3), total, "source 筛选")

	rows, total, err = repos.ListFunctionPrice(ctx, repository.ListQuery{Limit: 20}, nil, "SEARCH-")
	require.NoError(t, err)
	require.Equal(t, int64(3), total, "model 模糊（大小写不敏感；search- 前缀命中三行，不含 codex-search）")

	rows, total, err = repos.ListFunctionPrice(ctx, repository.ListQuery{Limit: 2, Offset: 0, Sort: "model", Order: "asc"}, nil, "")
	require.NoError(t, err)
	require.Equal(t, int64(4), total)
	require.Len(t, rows, 2, "分页第一页两行")

	_, _, err = repos.ListFunctionPrice(ctx, repository.ListQuery{Limit: 20, Sort: "bogus"}, nil, "")
	require.ErrorIs(t, err, repository.ErrInvalidSort, "sort 白名单外 → ErrInvalidSort")

	_, err = repos.GetFunctionPrice(ctx, "c3api-fn-missing")
	require.ErrorIs(t, err, repository.ErrNotFound)
}
