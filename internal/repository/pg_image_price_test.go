// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 真实 PG 基座（newPGRepos；TEST_DATABASE_URL 未设置 → Skip）：
// 图片生成价格 Task A 全部测试 —— image_price 表由 ent migrate 自动建表
// （无既有数据，无迁移测试——用户裁决 2026-08-12）、优先级闭环（manual >
// litellm）、DeleteManual 语义、分批/部分成功、并发死锁收敛（#37 P3' 同款）、
// List 筛选/分页/sort、raw JSONB 落库、与 pricings 双表独立并存。

// imageLitellmRow 构造拉取源 image 价行（三价格分量）。
func imageLitellmRow(model string, in, out, perImage int64) *domain.ImagePrice {
	return &domain.ImagePrice{
		Model:                           model,
		InputImageTokenPricePerMillion:  int64Ptr(in),
		OutputImageTokenPricePerMillion: int64Ptr(out),
		OutputCostPerImageMilli:         int64Ptr(perImage),
		Source:                          domain.PricingSourceLitellm,
	}
}

func imageManualReq(model string, in, out, perImage *int64) *repository.ImagePriceManual {
	return &repository.ImagePriceManual{
		Model:                           model,
		InputImageTokenPricePerMillion:  in,
		OutputImageTokenPricePerMillion: out,
		OutputCostPerImageMilli:         perImage,
	}
}

// TestImagePricePriorityPG 优先级闭环（行级互斥 manual > litellm，与 pricings
// 同款机制）：
// 1) 先手动后拉取 → 价格不变（DO UPDATE 被 WHERE source != 'manual' 过滤）；
// 2) 先拉取后手动 → 接管（source=manual 价格变）；
// 3) 删手动行后拉取 → 恢复拉取价。
func TestImagePricePriorityPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("litellm sync does not overwrite manual price", func(t *testing.T) {
		m := "c3api-img-pri-manual-a"
		p, err := repos.UpsertImageManual(ctx, imageManualReq(m, int64Ptr(100), nil, nil))
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source)
		require.Equal(t, m, p.Model)

		n, err := repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{imageLitellmRow(m, 999, 999, 999)})
		require.NoError(t, err)
		require.Zero(t, n, "manual 行被 WHERE 过滤，不产生更新")

		got, err := repos.GetImagePrice(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(100), *got.InputImageTokenPricePerMillion, "手动价不被覆盖")
		require.Nil(t, got.OutputImageTokenPricePerMillion, "手动行未设分量恒 nil")
		require.Equal(t, domain.PricingSourceManual, got.Source)
	})

	t.Run("manual upsert takes over litellm row", func(t *testing.T) {
		m := "c3api-img-pri-litellm-b"
		row := imageLitellmRow(m, 10, 20, 30)
		row.Provider = strPtrPG("openai")
		n, err := repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{row})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		got, err := repos.GetImagePrice(ctx, m)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceLitellm, got.Source)
		require.NotNil(t, got.Provider, "litellm 行带 provider")

		p, err := repos.UpsertImageManual(ctx, imageManualReq(m, int64Ptr(300), nil, nil))
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source)
		require.Equal(t, int64(300), *p.InputImageTokenPricePerMillion, "litellm 行被接管")

		got, err = repos.GetImagePrice(ctx, m)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, got.Source, "source 改为 manual")
		require.Nil(t, got.OutputCostPerImageMilli, "接管且不设 → 原 litellm 分量清空")
		require.Nil(t, got.Provider, "manual 接管后 provider 清为 NULL（S-2）")
		require.Nil(t, got.Raw, "manual 接管后 raw 清为 NULL")
	})

	t.Run("delete manual restores litellm price on next sync", func(t *testing.T) {
		m := "c3api-img-pri-restore-c"
		_, err := repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{imageLitellmRow(m, 10, 20, 30)})
		require.NoError(t, err)
		_, err = repos.UpsertImageManual(ctx, imageManualReq(m, int64Ptr(500), nil, nil))
		require.NoError(t, err)
		require.NoError(t, repos.DeleteImageManual(ctx, m), "manual 行可删")

		_, err = repos.GetImagePrice(ctx, m)
		require.ErrorIs(t, err, repository.ErrNotFound, "删手动行后整行消失（下轮拉取补回）")

		n, err := repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{imageLitellmRow(m, 10, 20, 30)})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		got, err := repos.GetImagePrice(ctx, m)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceLitellm, got.Source)
		require.Equal(t, int64(10), *got.InputImageTokenPricePerMillion, "恢复拉取价")
	})
}

// TestImagePriceDeleteManualPG DeleteManual 语义：litellm 行 → ErrConflict
// （只允许删手动价）；不存在 → ErrNotFound；manual 行成功删除。
func TestImagePriceDeleteManualPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("litellm row is not deletable", func(t *testing.T) {
		m := "c3api-img-del-litellm"
		_, err := repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{imageLitellmRow(m, 1, 2, 3)})
		require.NoError(t, err)
		err = repos.DeleteImageManual(ctx, m)
		require.ErrorIs(t, err, repository.ErrConflict, "litellm 行 → ErrConflict")
		require.Contains(t, err.Error(), `model="c3api-img-del-litellm"`)
		_, err = repos.GetImagePrice(ctx, m)
		require.NoError(t, err, "litellm 行保留未被删除")
	})

	t.Run("missing model is not found", func(t *testing.T) {
		err := repos.DeleteImageManual(ctx, "c3api-img-no-such")
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.Contains(t, err.Error(), `model="c3api-img-no-such"`)
	})

	t.Run("manual row deleted", func(t *testing.T) {
		m := "c3api-img-del-manual"
		_, err := repos.UpsertImageManual(ctx, imageManualReq(m, int64Ptr(1), nil, nil))
		require.NoError(t, err)
		require.NoError(t, repos.DeleteImageManual(ctx, m))
		_, err = repos.GetImagePrice(ctx, m)
		require.ErrorIs(t, err, repository.ErrNotFound, "manual 行已删除")
	})
}

// TestImagePriceUpsertBatchPG 分批（评审 M-2 同款机制）：
//  1. 1200 行（3 批）全成功，成功数 = 总行数；三价格分量 roundtrip；
//  2. 批内含手动行 → WHERE 过滤不覆盖，成功数相应减少；
//  3. 部分成功：批 0 内同 model 重复 → 整批失败，其余批成功，返回成功条数 +
//     首个错误。
func TestImagePriceUpsertBatchPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	// 1) >500 行全成功 + 分量 roundtrip
	const total = 1200
	rows := make([]*domain.ImagePrice, 0, total)
	for i := 0; i < total; i++ {
		rows = append(rows, imageLitellmRow(fmt.Sprintf("litellm-img-%04d", i), int64(i), int64(i*2), int64(i*3)))
	}
	n, err := repos.UpsertImageFromLiteLLM(ctx, rows)
	require.NoError(t, err)
	require.Equal(t, total, n, "全部插入成功")

	got, err := repos.GetImagePrice(ctx, "litellm-img-0000")
	require.NoError(t, err)
	require.Equal(t, int64(0), *got.InputImageTokenPricePerMillion)
	require.Equal(t, int64(0), *got.OutputImageTokenPricePerMillion)
	require.Equal(t, int64(0), *got.OutputCostPerImageMilli)
	require.Equal(t, domain.PricingSourceLitellm, got.Source)

	got, err = repos.GetImagePrice(ctx, "litellm-img-0001")
	require.NoError(t, err)
	require.Equal(t, int64(2), *got.OutputImageTokenPricePerMillion)
	require.Equal(t, int64(3), *got.OutputCostPerImageMilli)

	// 2) 批内含手动行：DO UPDATE 被 WHERE 过滤，成功数 = 总行数 - 1
	manualModel := "litellm-img-0600"
	_, err = repos.UpsertImageManual(ctx, imageManualReq(manualModel, int64Ptr(777), nil, nil))
	require.NoError(t, err)
	n, err = repos.UpsertImageFromLiteLLM(ctx, rows)
	require.NoError(t, err)
	require.Equal(t, total-1, n, "manual 行被过滤不计入更新数")
	got, err = repos.GetImagePrice(ctx, manualModel)
	require.NoError(t, err)
	require.Equal(t, int64(777), *got.InputImageTokenPricePerMillion, "手动价不被覆盖")
	require.Equal(t, domain.PricingSourceManual, got.Source)

	// 3) 部分成功：批 0（500 行）内含同 model 两行 → 整批失败，批 1 成功
	dupRows := make([]*domain.ImagePrice, 0, 502)
	for i := 0; i < 502; i++ {
		m := fmt.Sprintf("litellm-img-dup-%04d", i)
		if i == 250 || i == 251 {
			m = "litellm-img-dup-0250" // 同批内重复 → PG 报 "cannot affect row a second time"
		}
		dupRows = append(dupRows, imageLitellmRow(m, int64(i), 0, 0))
	}
	n, err = repos.UpsertImageFromLiteLLM(ctx, dupRows)
	require.Error(t, err, "批 0 失败返回错误（部分成功可接受）")
	require.Equal(t, 2, n, "仅批 1（2 行）成功")
	got, err = repos.GetImagePrice(ctx, "litellm-img-dup-0500")
	require.NoError(t, err, "批 1 行已落库")
	require.Equal(t, int64(500), *got.InputImageTokenPricePerMillion)
	_, err = repos.GetImagePrice(ctx, "litellm-img-dup-0250")
	require.ErrorIs(t, err, repository.ErrNotFound, "批 0 整体失败无残留")
}

// TestImagePriceUpsertConcurrentNoDeadlockPG 并发死锁收敛（#37 P3' 同款治本，
// 对齐 TestPricingUpsertConcurrentNoDeadlockPG）：4 goroutine 并发 upsert 同一
// 批 500 model（2 正序 + 2 倒序）→ 断言无错误、行数精确不重复、值未被写乱。
func TestImagePriceUpsertConcurrentNoDeadlockPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	const n = 500
	rows := make([]*domain.ImagePrice, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, imageLitellmRow(fmt.Sprintf("litellm-img-conc-%04d", i), int64(i), int64(i*2), int64(i*3)))
	}
	_, err := repos.UpsertImageFromLiteLLM(ctx, rows)
	require.NoError(t, err, "预置存量行（并发 DO UPDATE 冲突路径）")

	rev := make([]*domain.ImagePrice, len(rows))
	for i := range rows {
		rev[len(rows)-1-i] = rows[i]
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			src := rows
			if i%2 == 1 {
				src = rev
			}
			b2 := make([]*domain.ImagePrice, len(src))
			copy(b2, src)
			_, errs[i] = repos.UpsertImageFromLiteLLM(ctx, b2)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		require.NoError(t, e, "并发 upsert 同批 model 无 deadlock（排序 + 重试兜底）", i)
	}

	total, err := repos.Client.ImagePrice.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, n, total, "500 行不重复计（DO UPDATE 覆盖非新增）")
	for _, idx := range []int{0, 250, 499} {
		got, err := repos.GetImagePrice(ctx, fmt.Sprintf("litellm-img-conc-%04d", idx))
		require.NoError(t, err)
		require.Equal(t, int64(idx), *got.InputImageTokenPricePerMillion, "model=%q input 价精确", got.Model)
		require.Equal(t, int64(idx*3), *got.OutputCostPerImageMilli, "model=%q per-image 价精确", got.Model)
	}
}

// TestImagePriceListPG 列表：全量分页 / source 筛选 / model 模糊 / sort 白名单
// （model/updated_at 合法，非法 → ErrInvalidSort）。
func TestImagePriceListPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	litellmRows := []*domain.ImagePrice{
		imageLitellmRow("gpt-image-2", 800000, 3000000, 0),
		imageLitellmRow("aiml-image", 0, 0, 5400),
	}
	litellmRows[0].Provider = strPtrPG("openai")  // litellm 行带 provider（拉取直贴）
	litellmRows[1].Provider = strPtrPG("alibaba") // aiml 为阿里系
	_, err := repos.UpsertImageFromLiteLLM(ctx, litellmRows)
	require.NoError(t, err)
	_, err = repos.UpsertImageManual(ctx, imageManualReq("dall-e-3", int64Ptr(1), nil, nil))
	require.NoError(t, err)

	// 全量（默认分页 id desc）
	rows, total, err := repos.ListImagePrice(ctx, repository.ListQuery{Limit: 10}, nil, "", "")
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, rows, 3)

	// 筛选 source=manual
	ms := domain.PricingSourceManual
	rows, total, err = repos.ListImagePrice(ctx, repository.ListQuery{}, &ms, "", "")
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, domain.PricingSourceManual, rows[0].Source)

	// model 模糊（ilike，不区分大小写）：image → 2 行
	_, total, err = repos.ListImagePrice(ctx, repository.ListQuery{}, nil, "", "image")
	require.NoError(t, err)
	require.Equal(t, int64(2), total)

	// 筛选 provider=openai → 1 行（manual 行无 provider 不命中）
	rows, total, err = repos.ListImagePrice(ctx, repository.ListQuery{}, nil, "openai", "")
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, "gpt-image-2", rows[0].Model)
	require.NotNil(t, rows[0].Provider)
	require.Equal(t, "openai", *rows[0].Provider, "litellm 行回显 provider")

	// 筛选 provider=alibaba → 1 行（非 openai 主流形态亦可筛）
	_, total, err = repos.ListImagePrice(ctx, repository.ListQuery{}, nil, "alibaba", "")
	require.NoError(t, err)
	require.Equal(t, int64(1), total)

	// provider 不命中 → 0 行
	_, total, err = repos.ListImagePrice(ctx, repository.ListQuery{}, nil, "bedrock", "")
	require.NoError(t, err)
	require.Zero(t, total)

	// 分页 + sort model asc
	rows, total, err = repos.ListImagePrice(ctx, repository.ListQuery{Limit: 2, Offset: 0, Sort: "model", Order: "asc"}, nil, "", "")
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, rows, 2)
	require.Equal(t, "aiml-image", rows[0].Model)
	require.Equal(t, "dall-e-3", rows[1].Model)

	// sort updated_at desc 合法
	rows, total, err = repos.ListImagePrice(ctx, repository.ListQuery{Limit: 10, Sort: "updated_at", Order: "desc"}, nil, "", "")
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, rows, 3)

	// 非法 sort → ErrInvalidSort
	_, _, err = repos.ListImagePrice(ctx, repository.ListQuery{Sort: "bogus"}, nil, "", "")
	require.ErrorIs(t, err, repository.ErrInvalidSort)
}

// TestImagePriceRawAndManualPG raw JSONB 落库 + manual 行恒无 raw（对齐
// TestPricingCacheAndMetaFieldsPG 的 raw 语义）；与 pricings 双表独立并存
// （同 model 名可同时有 pricings 行与 image_price 行）。
func TestImagePriceRawAndManualPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("litellm raw roundtrip", func(t *testing.T) {
		m := "c3api-img-raw-a"
		row := imageLitellmRow(m, 100, 200, 300)
		row.Raw = json.RawMessage(`{"input_cost_per_image_token":1e-06,"rpm":600,"mode":"image_generation"}`)
		n, err := repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{row})
		require.NoError(t, err)
		require.Equal(t, 1, n)

		got, err := repos.GetImagePrice(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(100), *got.InputImageTokenPricePerMillion)
		require.Equal(t, int64(300), *got.OutputCostPerImageMilli)
		require.NotNil(t, got.Raw, "raw JSONB 落库")
		var raw map[string]any
		require.NoError(t, json.Unmarshal(got.Raw, &raw))
		require.Equal(t, float64(600), raw["rpm"], "raw 保留未映射字段")
	})

	t.Run("manual row has no raw", func(t *testing.T) {
		m := "c3api-img-raw-manual"
		p, err := repos.UpsertImageManual(ctx, imageManualReq(m, int64Ptr(5), nil, nil))
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source)
		require.Nil(t, p.Raw, "manual 行 raw 恒 NULL")
		got, err := repos.GetImagePrice(ctx, m)
		require.NoError(t, err)
		require.Nil(t, got.Raw, "manual 行 raw=NULL 落库")
	})

	t.Run("dual table independent coexistence", func(t *testing.T) {
		// 同 model 名：pricings（文本价）+ image_price（image 价）双行并存
		m := "c3api-img-dual-table"
		_, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{
			{Model: m, PromptPricePerMillion: 250000, CompletionPricePerMillion: 1000000, Source: domain.PricingSourceLitellm},
		})
		require.NoError(t, err)
		_, err = repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{imageLitellmRow(m, 800000, 3000000, 5400)})
		require.NoError(t, err)

		pp, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(250000), pp.PromptPricePerMillion, "pricings 行存在")
		ip, err := repos.GetImagePrice(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(800000), *ip.InputImageTokenPricePerMillion, "image_price 行存在")
		require.Equal(t, int64(5400), *ip.OutputCostPerImageMilli)
	})
}

// TestImagePriceProviderPG provider 元数据列（spec 三价格表厂商筛选）：
// litellm 拉取行 provider 落库 roundtrip（S-1 raw SQL 列清单验证——列缺失即
// 整批失败）；无 litellm_provider 条目 → provider nil；manual 行（新行与接管
// litellm 行）provider 恒 nil（S-2）；provider 等值筛选（命中/不命中/nil 不筛/
// 非 enum 自由字符串可筛——litellm_provider 动态，DB 侧不受 openapi enum 约束）。
func TestImagePriceProviderPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("schema provider column exists on both tables", func(t *testing.T) {
		// 显式 schema 断言（非分区表 ent migrate 自动 ADD COLUMN，无 DROP 重建）：
		// 存量手动行新增列默认 NULL（manual 行 nil 语义由下方接管/手动测试覆盖）。
		db := pgTestDB(t)
		var cnt int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.columns
			 WHERE table_schema = 'public' AND column_name = 'provider'
			   AND table_name IN ('image_prices', 'function_prices')`).Scan(&cnt)
		require.NoError(t, err)
		require.Equal(t, 2, cnt, "image_prices/function_prices 两表均含 provider 列（可空）")
	})

	t.Run("litellm provider column roundtrip", func(t *testing.T) {
		row := imageLitellmRow("c3api-img-prov-a", 100, 200, 300)
		row.Provider = strPtrPG("openai")
		n, err := repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{row})
		require.NoError(t, err)
		require.Equal(t, 1, n)

		got, err := repos.GetImagePrice(ctx, "c3api-img-prov-a")
		require.NoError(t, err)
		require.NotNil(t, got.Provider, "litellm 行 provider 落库")
		require.Equal(t, "openai", *got.Provider)

		// 无 litellm_provider 条目 → NULL（拉取直贴语义：缺失即 nil）
		_, err = repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{imageLitellmRow("c3api-img-prov-nil", 1, 2, 3)})
		require.NoError(t, err)
		got, err = repos.GetImagePrice(ctx, "c3api-img-prov-nil")
		require.NoError(t, err)
		require.Nil(t, got.Provider, "条目无 litellm_provider → nil")
	})

	t.Run("manual rows and takeover keep provider nil", func(t *testing.T) {
		// 新 manual 行：无厂商概念 → provider nil
		p, err := repos.UpsertImageManual(ctx, imageManualReq("c3api-img-prov-manual", int64Ptr(5), nil, nil))
		require.NoError(t, err)
		require.Nil(t, p.Provider, "manual 行 provider 恒 nil")

		// S-2 接管：litellm 行带 provider → manual 接管后清 provider
		m := "c3api-img-prov-takeover"
		row := imageLitellmRow(m, 10, 20, 30)
		row.Provider = strPtrPG("anthropic")
		_, err = repos.UpsertImageFromLiteLLM(ctx, []*domain.ImagePrice{row})
		require.NoError(t, err)
		got, err := repos.GetImagePrice(ctx, m)
		require.NoError(t, err)
		require.Equal(t, "anthropic", *got.Provider)

		p, err = repos.UpsertImageManual(ctx, imageManualReq(m, int64Ptr(300), nil, nil))
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source)
		require.Nil(t, p.Provider, "manual 接管清 provider（S-2）")
	})

	t.Run("provider equality filter", func(t *testing.T) {
		rows := []*domain.ImagePrice{
			imageLitellmRow("c3api-img-f-openai", 1, 2, 3),
			imageLitellmRow("c3api-img-f-anthropic", 4, 5, 6),
			imageLitellmRow("c3api-img-f-future", 7, 8, 9),
		}
		rows[0].Provider = strPtrPG("openai")
		rows[1].Provider = strPtrPG("anthropic")
		rows[2].Provider = strPtrPG("some_future_vendor") // 非 enum 值（litellm 未来新厂商）
		_, err := repos.UpsertImageFromLiteLLM(ctx, rows)
		require.NoError(t, err)
		_, err = repos.UpsertImageManual(ctx, imageManualReq("c3api-img-f-manual", int64Ptr(1), nil, nil))
		require.NoError(t, err)

		// 等值命中：本子测试 1 行 + roundtrip 子测试的 openai 行 = 2
		got, total, err := repos.ListImagePrice(ctx, repository.ListQuery{Limit: 10, Sort: "model", Order: "asc"}, nil, "openai", "")
		require.NoError(t, err)
		require.Equal(t, int64(2), total)
		require.Equal(t, "c3api-img-f-openai", got[0].Model)

		// 不命中（无该厂商行）
		_, total, err = repos.ListImagePrice(ctx, repository.ListQuery{Limit: 10}, nil, "bedrock", "")
		require.NoError(t, err)
		require.Zero(t, total)

		// nil 不筛（全量 = 前序子测试 4 行 + 本子测试 4 行）
		_, total, err = repos.ListImagePrice(ctx, repository.ListQuery{Limit: 10}, nil, "", "")
		require.NoError(t, err)
		require.Equal(t, int64(8), total)

		// 非 enum 自由字符串可筛（litellm_provider 动态，DB 不受枚举约束）
		got, total, err = repos.ListImagePrice(ctx, repository.ListQuery{Limit: 10}, nil, "some_future_vendor", "")
		require.NoError(t, err)
		require.Equal(t, int64(1), total)
		require.Equal(t, "c3api-img-f-future", got[0].Model)

		// 与 source 筛选组合
		litellmSrc := domain.PricingSourceLitellm
		_, total, err = repos.ListImagePrice(ctx, repository.ListQuery{Limit: 10}, &litellmSrc, "openai", "")
		require.NoError(t, err)
		require.Equal(t, int64(2), total)
	})
}
