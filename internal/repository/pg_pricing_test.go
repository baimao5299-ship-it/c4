package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// 真实 PG 基座（newPGRepos；TEST_DATABASE_URL 未设置 → Skip）：
// 模型价格 Task 1 全部测试 —— 优先级闭环（manual > litellm）、DeleteManual
// 语义、UpsertFromLiteLLM 分批/部分成功（评审 M-2）、ListPricing 筛选/分页/sort；
// T5/T5b —— cache 价 + provider/mode/supports_prompt_caching/raw JSONB 落库。

func int64Ptr(v int64) *int64 { return &v }

// manualReq 构造 PricingManual（可选 cache 价；矩阵字段走显式设置）。
func manualReq(model string, prompt, completion int64, cache ...*int64) *repository.PricingManual {
	m := &repository.PricingManual{
		Model: model, PromptPricePerMillion: prompt, CompletionPricePerMillion: completion,
	}
	if len(cache) >= 1 {
		m.CacheReadPricePerMillion = cache[0]
	}
	if len(cache) >= 2 {
		m.CacheCreationPricePerMillion = cache[1]
	}
	return m
}

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
		p, err := repos.UpsertManual(ctx, manualReq(m, 100, 200))
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

		p, err := repos.UpsertManual(ctx, manualReq(m, 300, 400))
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
		_, err = repos.UpsertManual(ctx, manualReq(m, 500, 600))
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
		_, err := repos.UpsertManual(ctx, manualReq(m, 1, 2))
		require.NoError(t, err)
		require.NoError(t, repos.DeleteManual(ctx, m))
		_, err = repos.GetPricing(ctx, m)
		require.ErrorIs(t, err, repository.ErrNotFound, "manual 行已删除")
	})
}

// TestPricingUpsertLitellmBatchPG 分批（评审 M-2）：
//  1. 1200 行（3 批）全成功，成功数 = 总行数，max_tokens 空/非空 roundtrip；
//  2. 批内含手动行 → WHERE 过滤不覆盖，成功数相应减少；
//  3. 部分成功：批 0 内同 model 重复（同批互冲突 → 整批失败）→ 其余批成功，
//     返回成功条数 + 首个错误。
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
	_, err = repos.UpsertManual(ctx, manualReq(manualModel, 777, 888))
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

// TestPricingUpsertConcurrentNoDeadlockPG 并发死锁收敛（#37 P3' 同款治本，
// 对齐 TestPGStatUpsertConcurrentNoDeadlock）：4 goroutine 并发 upsert 同一批
// 500 model（2 正序 + 2 倒序，起跑屏障最大化锁获取重叠；无批内排序必
// deadlock detected 40P01）→ 断言无错误、行数精确不重复、值未被写乱。
func TestPricingUpsertConcurrentNoDeadlockPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	const n = 500 // 单批上限同量级（锁重叠窗口足够大，无修复必死锁）
	rows := make([]*domain.Pricing, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, litellmRow(fmt.Sprintf("litellm-conc-%04d", i), int64(i), int64(i*2)))
	}
	_, err := repos.UpsertFromLiteLLM(ctx, rows)
	require.NoError(t, err, "预置存量行（并发 DO UPDATE 冲突路径）")

	rev := make([]*domain.Pricing, len(rows)) // 实例 B：行序相反（map 随机迭代极端形态）
	for i := range rows {
		rev[len(rows)-1-i] = rows[i]
	}

	start := make(chan struct{}) // 起跑屏障：最大化两批锁获取重叠
	var wg sync.WaitGroup
	errs := make([]error, 4) // 2 正序 + 2 倒序（多 worker 多实例形态）
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			src := rows
			if i%2 == 1 {
				src = rev
			}
			b2 := make([]*domain.Pricing, len(src)) // 每 goroutine 独立副本：避免并发排序同一数组（评审 M-1）
			copy(b2, src)
			_, errs[i] = repos.UpsertFromLiteLLM(ctx, b2)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		require.NoError(t, e, "并发 upsert 同批 model 无 deadlock（排序 + 重试兜底）", i)
	}

	total, err := repos.Client.Pricing.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, n, total, "500 行不重复计（DO UPDATE 覆盖非新增）")
	for _, idx := range []int{0, 250, 499} { // 抽查首/中/尾行值未被并发写乱
		got, err := repos.GetPricing(ctx, fmt.Sprintf("litellm-conc-%04d", idx))
		require.NoError(t, err)
		require.Equal(t, int64(idx), got.PromptPricePerMillion, "model=%q prompt 价精确", got.Model)
		require.Equal(t, int64(idx*2), got.CompletionPricePerMillion, "model=%q completion 价精确", got.Model)
	}
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
	_, err = repos.UpsertManual(ctx, manualReq("gpt-4o-mini", 5, 6))
	require.NoError(t, err)
	_, err = repos.UpsertManual(ctx, manualReq("gemini-pro", 7, 8))
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

// TestPricingCacheAndMetaFieldsPG T5/T5b 字段落库闭环：
//  1. litellm 行 cache 价 + provider/mode/supports_prompt_caching 落库 roundtrip；
//     缺 cache 价行 → nil；
//  2. 再拉取更新（cache 价变化）→ DO UPDATE 覆盖（非 manual 行）；
//  3. manual 设 cache 价落库；manual 不设（nil）→ 落库 NULL；
//  4. manual 接管带 cache 价的 litellm 行且不设 cache → 缓存价被清为 NULL。
func TestPricingCacheAndMetaFieldsPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("litellm row with cache/meta fields roundtrip", func(t *testing.T) {
		m := "gpm-cache-a"
		row := litellmRow(m, 100, 200)
		row.CacheReadPricePerMillion = int64Ptr(300)
		row.CacheCreationPricePerMillion = int64Ptr(400)
		prov, mode, spc := "openai", "chat", true
		row.Provider, row.Mode, row.SupportsPromptCaching = &prov, &mode, &spc
		row.Raw = json.RawMessage(`{"input_cost_per_token":1e-06,"rpm":600,"supports_vision":true}`)
		n, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{row})
		require.NoError(t, err)
		require.Equal(t, 1, n)

		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.NotNil(t, got.CacheReadPricePerMillion)
		require.Equal(t, int64(300), *got.CacheReadPricePerMillion, "cache_read 落库")
		require.NotNil(t, got.CacheCreationPricePerMillion)
		require.Equal(t, int64(400), *got.CacheCreationPricePerMillion)
		require.NotNil(t, got.Provider)
		require.Equal(t, "openai", *got.Provider)
		require.NotNil(t, got.Mode)
		require.Equal(t, "chat", *got.Mode)
		require.NotNil(t, got.SupportsPromptCaching)
		require.True(t, *got.SupportsPromptCaching)
		require.Equal(t, domain.PricingSourceLitellm, got.Source)

		// raw JSONB roundtrip：完整镜像保留未映射字段
		require.NotNil(t, got.Raw, "raw JSONB 落库")
		var raw map[string]any
		require.NoError(t, json.Unmarshal(got.Raw, &raw))
		require.Equal(t, float64(600), raw["rpm"], "raw 保留未映射字段")
		require.Equal(t, true, raw["supports_vision"], "raw 保留未映射字段")

		// 无 cache 价行 → nil
		n, err = repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{litellmRow("gpm-cache-b", 1, 2)})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		got, err = repos.GetPricing(ctx, "gpm-cache-b")
		require.NoError(t, err)
		require.Nil(t, got.CacheReadPricePerMillion)
		require.Nil(t, got.CacheCreationPricePerMillion)
		require.Nil(t, got.Provider)
		require.Nil(t, got.Raw, "无 raw → NULL")
	})

	t.Run("re-upsert updates cache/meta/raw", func(t *testing.T) {
		m := "gpm-cache-c"
		row := litellmRow(m, 10, 20)
		row.CacheReadPricePerMillion = int64Ptr(111)
		prov := "openai"
		row.Provider = &prov
		row.Raw = json.RawMessage(`{"k":"v1"}`)
		_, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{row})
		require.NoError(t, err)

		row2 := litellmRow(m, 10, 20)
		row2.CacheReadPricePerMillion = int64Ptr(222)
		row2.CacheCreationPricePerMillion = int64Ptr(333)
		prov2 := "anthropic"
		row2.Provider = &prov2
		row2.Raw = json.RawMessage(`{"k":"v2","extra":true}`)
		n, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{row2})
		require.NoError(t, err)
		require.Equal(t, 1, n)

		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(222), *got.CacheReadPricePerMillion, "cache 价更新")
		require.Equal(t, int64(333), *got.CacheCreationPricePerMillion)
		require.Equal(t, "anthropic", *got.Provider, "provider 更新")
		var raw map[string]any
		require.NoError(t, json.Unmarshal(got.Raw, &raw))
		require.Equal(t, true, raw["extra"], "raw 更新为最新镜像")
	})

	t.Run("manual with cache prices", func(t *testing.T) {
		m := "gpm-cache-manual"
		p, err := repos.UpsertManual(ctx, manualReq(m, 5, 6, int64Ptr(7), int64Ptr(8)))
		require.NoError(t, err)
		require.Equal(t, int64(7), *p.CacheReadPricePerMillion, "manual 返回行含 cache 价")
		require.Equal(t, int64(8), *p.CacheCreationPricePerMillion)
		require.Nil(t, p.Provider, "manual 行无 provider")
		require.Nil(t, p.Raw, "manual 行 raw 恒 NULL")
		require.Equal(t, domain.PricingSourceManual, p.Source)

		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(7), *got.CacheReadPricePerMillion, "manual cache 价落库")
		require.Nil(t, got.Raw, "manual 行 raw=NULL 落库")
	})

	t.Run("manual without cache prices stores NULL", func(t *testing.T) {
		m := "gpm-cache-manual-nil"
		p, err := repos.UpsertManual(ctx, manualReq(m, 5, 6))
		require.NoError(t, err)
		require.Nil(t, p.CacheReadPricePerMillion, "不设 cache 价 → nil")
		require.Nil(t, p.CacheCreationPricePerMillion)

		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Nil(t, got.CacheReadPricePerMillion, "落库 NULL")
		require.Nil(t, got.CacheCreationPricePerMillion)
	})

	t.Run("manual takeover clears litellm cache prices when unset", func(t *testing.T) {
		m := "gpm-cache-takeover"
		row := litellmRow(m, 10, 20)
		row.CacheReadPricePerMillion = int64Ptr(999)
		row.CacheCreationPricePerMillion = int64Ptr(888)
		row.Raw = json.RawMessage(`{"input_cost_per_token":1e-06}`)
		_, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{row})
		require.NoError(t, err)

		// 接管且不设 cache 价 → 原 litellm 缓存价清为 NULL
		_, err = repos.UpsertManual(ctx, manualReq(m, 1, 2))
		require.NoError(t, err)
		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, got.Source)
		require.Nil(t, got.CacheReadPricePerMillion, "接管不设 cache → NULL")
		require.Nil(t, got.CacheCreationPricePerMillion)
		require.Nil(t, got.Raw, "manual 接管后 raw 清为 NULL")
	})
}

// TestPricingMatrixFieldsPG Phase 5 矩阵 22 列落库闭环：
//  1. litellm 行全矩阵（priority/flex/above 三组/fast）roundtrip；缺矩阵行 → nil；
//  2. 再拉取更新（矩阵变化）→ DO UPDATE 覆盖（非 manual 行），缺失列置 NULL；
//  3. manual 设矩阵价落库；manual 接管带矩阵的 litellm 行且不设 → 矩阵清为 NULL。
func TestPricingMatrixFieldsPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("litellm row full matrix roundtrip", func(t *testing.T) {
		m := "gpm-matrix-a"
		row := litellmRow(m, 100, 200)
		row.PriorityPromptPricePerMillion = int64Ptr(110)
		row.PriorityCompletionPricePerMillion = int64Ptr(220)
		row.PriorityCacheReadPricePerMillion = int64Ptr(330)
		row.PriorityCacheCreationPricePerMillion = int64Ptr(440)
		row.FlexPromptPricePerMillion = int64Ptr(90)
		row.FlexCompletionPricePerMillion = int64Ptr(180)
		row.FlexCacheReadPricePerMillion = int64Ptr(270)
		row.FlexCacheCreationPricePerMillion = int64Ptr(360)
		row.AboveThreshold = int64Ptr(272000)
		row.AbovePromptPricePerMillion = int64Ptr(80)
		row.AboveCompletionPricePerMillion = int64Ptr(160)
		row.AboveCacheReadPricePerMillion = int64Ptr(240)
		row.AboveCacheCreationPricePerMillion = int64Ptr(320)
		row.AbovePriorityPromptPricePerMillion = int64Ptr(70)
		row.AbovePriorityCompletionPricePerMillion = int64Ptr(140)
		row.AbovePriorityCacheReadPricePerMillion = int64Ptr(210)
		row.AbovePriorityCacheCreationPricePerMillion = int64Ptr(280)
		row.AboveFlexPromptPricePerMillion = int64Ptr(60)
		row.AboveFlexCompletionPricePerMillion = int64Ptr(120)
		row.AboveFlexCacheReadPricePerMillion = int64Ptr(180)
		row.AboveFlexCacheCreationPricePerMillion = int64Ptr(240)
		row.FastMultiplier = int64Ptr(60000)
		n, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{row})
		require.NoError(t, err)
		require.Equal(t, 1, n)

		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(110), *got.PriorityPromptPricePerMillion, "priority 矩阵落库")
		require.Equal(t, int64(440), *got.PriorityCacheCreationPricePerMillion)
		require.Equal(t, int64(90), *got.FlexPromptPricePerMillion, "flex 矩阵落库")
		require.Equal(t, int64(360), *got.FlexCacheCreationPricePerMillion)
		require.Equal(t, int64(272000), *got.AboveThreshold, "above 阈值落库")
		require.Equal(t, int64(80), *got.AbovePromptPricePerMillion, "above 基础组落库")
		require.Equal(t, int64(320), *got.AboveCacheCreationPricePerMillion)
		require.Equal(t, int64(70), *got.AbovePriorityPromptPricePerMillion, "above_priority 组落库")
		require.Equal(t, int64(280), *got.AbovePriorityCacheCreationPricePerMillion)
		require.Equal(t, int64(60), *got.AboveFlexPromptPricePerMillion, "above_flex 组落库")
		require.Equal(t, int64(240), *got.AboveFlexCacheCreationPricePerMillion)
		require.Equal(t, int64(60000), *got.FastMultiplier, "fast 万分数落库")
		require.Equal(t, domain.PricingSourceLitellm, got.Source)

		// 无矩阵行 → nil
		n, err = repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{litellmRow("gpm-matrix-b", 1, 2)})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		got, err = repos.GetPricing(ctx, "gpm-matrix-b")
		require.NoError(t, err)
		require.Nil(t, got.PriorityPromptPricePerMillion)
		require.Nil(t, got.FastMultiplier)
		require.Nil(t, got.AboveThreshold)
	})

	t.Run("re-upsert overwrites matrix and clears missing cols", func(t *testing.T) {
		m := "gpm-matrix-c"
		row := litellmRow(m, 10, 20)
		row.PriorityPromptPricePerMillion = int64Ptr(111)
		row.FastMultiplier = int64Ptr(20000)
		_, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{row})
		require.NoError(t, err)

		// 第二次拉取：矩阵变化 + 缺失列 → NULL
		row2 := litellmRow(m, 10, 20)
		row2.PriorityPromptPricePerMillion = int64Ptr(222)
		row2.AboveThreshold = int64Ptr(200000)
		n, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{row2})
		require.NoError(t, err)
		require.Equal(t, 1, n)

		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(222), *got.PriorityPromptPricePerMillion, "矩阵更新")
		require.Equal(t, int64(200000), *got.AboveThreshold)
		require.Nil(t, got.FastMultiplier, "缺失矩阵列 → NULL（计费回退语义）")
	})

	t.Run("manual matrix prices roundtrip", func(t *testing.T) {
		m := "gpm-matrix-manual"
		req := manualReq(m, 5, 6)
		req.PriorityPromptPricePerMillion = int64Ptr(50)
		req.PriorityCompletionPricePerMillion = int64Ptr(60)
		req.FlexPromptPricePerMillion = int64Ptr(40)
		req.FlexCompletionPricePerMillion = int64Ptr(48)
		req.AboveThreshold = int64Ptr(100000)
		req.AbovePromptPricePerMillion = int64Ptr(30)
		req.AboveCompletionPricePerMillion = int64Ptr(36)
		req.AboveFlexPromptPricePerMillion = int64Ptr(25)
		req.AboveFlexCompletionPricePerMillion = int64Ptr(30)
		req.FastMultiplier = int64Ptr(20000)
		p, err := repos.UpsertManual(ctx, req)
		require.NoError(t, err)
		require.Equal(t, int64(50), *p.PriorityPromptPricePerMillion, "manual 返回行含矩阵")
		require.Equal(t, int64(25), *p.AboveFlexPromptPricePerMillion)
		require.Equal(t, int64(20000), *p.FastMultiplier)
		require.Nil(t, p.AbovePriorityPromptPricePerMillion, "未设置组 → nil")

		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(50), *got.PriorityPromptPricePerMillion, "manual 矩阵落库")
		require.Equal(t, int64(100000), *got.AboveThreshold)
		require.Equal(t, int64(20000), *got.FastMultiplier)
	})

	t.Run("manual takeover clears litellm matrix when unset", func(t *testing.T) {
		m := "gpm-matrix-takeover"
		row := litellmRow(m, 10, 20)
		row.PriorityPromptPricePerMillion = int64Ptr(999)
		row.FlexPromptPricePerMillion = int64Ptr(888)
		row.AboveThreshold = int64Ptr(272000)
		row.AbovePromptPricePerMillion = int64Ptr(777)
		row.FastMultiplier = int64Ptr(60000)
		_, err := repos.UpsertFromLiteLLM(ctx, []*domain.Pricing{row})
		require.NoError(t, err)

		// 接管且不设矩阵 → 原 litellm 矩阵全清为 NULL
		_, err = repos.UpsertManual(ctx, manualReq(m, 1, 2))
		require.NoError(t, err)
		got, err := repos.GetPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, got.Source)
		require.Nil(t, got.PriorityPromptPricePerMillion, "接管不设 priority → NULL")
		require.Nil(t, got.FlexPromptPricePerMillion)
		require.Nil(t, got.AboveThreshold)
		require.Nil(t, got.AbovePromptPricePerMillion)
		require.Nil(t, got.FastMultiplier, "接管不设 fast → NULL")
	})
}
