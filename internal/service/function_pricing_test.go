// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
)

// newFunctionPricingSvc 构造 Service + 显式首刷三线价格快照（文本价 + image 价
// + 按单元价，对齐 newPricingSvc 语义——New 不自载，首刷由注册表 ReloadAll 承担）。
func newFunctionPricingSvc(t *testing.T, fs *fakeStore) *Service {
	t.Helper()
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	require.NoError(t, svc.ReloadImagePricingCtx(context.Background()))
	require.NoError(t, svc.ReloadFunctionPricingCtx(context.Background()))
	return svc
}

func functionLitellmRow(model string, perCall int64) *domain.FunctionPrice {
	return &domain.FunctionPrice{
		Model:        model,
		PricePerCall: int64Ptr(perCall),
		Source:       domain.PricingSourceLitellm,
	}
}

func functionManualReq(model string, perCall *int64) *repository.FunctionPriceManual {
	return &repository.FunctionPriceManual{Model: model, PricePerCall: perCall}
}

// TestFunctionPriceSnapshotLoadNew 快照首载：GetFunctionPrice 命中（litellm +
// manual 行）；查无 codex-search → 默认兜底行（$0.01/次）；查无其他 →
// ErrNotFound（计费方拒绝语义）。
func TestFunctionPriceSnapshotLoadNew(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFunctionFromLiteLLM(context.Background(), []*domain.FunctionPrice{
		functionLitellmRow("search-alpha", 10),
	})
	require.NoError(t, err)
	_, err = fs.UpsertFunctionManual(context.Background(), functionManualReq("codex-search", int64Ptr(1000)))
	require.NoError(t, err)

	svc := newFunctionPricingSvc(t, fs)

	p, err := svc.GetFunctionPrice("search-alpha")
	require.NoError(t, err)
	require.Equal(t, int64(10), *p.PricePerCall)
	require.Equal(t, domain.PricingSourceLitellm, p.Source)

	p, err = svc.GetFunctionPrice("codex-search")
	require.NoError(t, err)
	require.Equal(t, int64(1000), *p.PricePerCall)
	require.Equal(t, domain.PricingSourceManual, p.Source)

	_, err = svc.GetFunctionPrice("no-such-model")
	require.ErrorIs(t, err, ErrNotFound, "查无其他 → 错误（计费方拒绝）")
}

// TestFunctionPriceDefaultFallback 默认兜底（表删/初始化失败/快照为空）：
// GetFunctionPrice("codex-search") 返回默认价行（1000 毫分 = $0.01/次、
// source=manual）；其余模型 → ErrNotFound。每次调用返回新建行（防调用方
// 原地修改污染快照语义）。
func TestFunctionPriceDefaultFallback(t *testing.T) {
	fs := newFakeStore()
	svc := newFunctionPricingSvc(t, fs) // function_price 表空

	p, err := svc.GetFunctionPrice(domain.CodexSearchModel)
	require.NoError(t, err, "codex-search 查无 → 默认价行（防御语义）")
	require.NotNil(t, p.PricePerCall)
	require.Equal(t, domain.DefaultCodexSearchPricePerCall, *p.PricePerCall, "$0.01/次 = 1000 毫分")
	require.Equal(t, domain.PricingSourceManual, p.Source)

	// 每次调用新建行：修改返回值不影响后续读取
	*p.PricePerCall = 1
	p2, err := svc.GetFunctionPrice(domain.CodexSearchModel)
	require.NoError(t, err)
	require.Equal(t, domain.DefaultCodexSearchPricePerCall, *p2.PricePerCall, "默认行不可变")

	_, err = svc.GetFunctionPrice("other-model")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestFunctionPriceSnapshotReload ReloadFunctionPricing 后读路径即时生效（零 DB）。
func TestFunctionPriceSnapshotReload(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFunctionFromLiteLLM(context.Background(), []*domain.FunctionPrice{
		functionLitellmRow("search-alpha", 10),
	})
	require.NoError(t, err)
	svc := newFunctionPricingSvc(t, fs)

	// 库内新增行：快照未刷新前读不到
	_, err = fs.UpsertFunctionManual(context.Background(), functionManualReq("search-b", int64Ptr(20)))
	require.NoError(t, err)
	_, err = svc.GetFunctionPrice("search-b")
	require.ErrorIs(t, err, ErrNotFound, "快照未刷新（DB 新增不自动可见）")

	svc.ReloadFunctionPricing()
	p, err := svc.GetFunctionPrice("search-b")
	require.NoError(t, err)
	require.Equal(t, int64(20), *p.PricePerCall)
}

// TestUpsertManualFunctionPrice 管理端手动设按单元价：校验（model 空 /
// price_per_call nil → 400；负数 → 400）+ 成功后自动重载快照（读路径即时生效）。
func TestUpsertManualFunctionPrice(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFunctionFromLiteLLM(context.Background(), []*domain.FunctionPrice{
		functionLitellmRow("search-alpha", 10),
	})
	require.NoError(t, err)
	svc := newFunctionPricingSvc(t, fs)
	ctx := context.Background()

	t.Run("validation", func(t *testing.T) {
		_, err := svc.UpsertManualFunctionPrice(ctx, functionManualReq("", int64Ptr(1)))
		require.ErrorIs(t, err, ErrInvalidInput, "model 空 → 400")
		_, err = svc.UpsertManualFunctionPrice(ctx, functionManualReq("m", nil))
		require.ErrorIs(t, err, ErrInvalidInput, "price_per_call nil → 400（行有效性 = 按单元价非 nil）")
		_, err = svc.UpsertManualFunctionPrice(ctx, functionManualReq("m", int64Ptr(-1)))
		require.ErrorIs(t, err, ErrInvalidInput, "负价 → 400")
		_, err = svc.UpsertManualFunctionPrice(ctx, functionManualReq("m", int64Ptr(0)))
		require.NoError(t, err, "显式 0 价 = 按次免费，允许")
	})

	t.Run("success reloads snapshot", func(t *testing.T) {
		p, err := svc.UpsertManualFunctionPrice(ctx, functionManualReq("search-alpha", int64Ptr(500)))
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source, "接管 litellm 行")
		got, err := svc.GetFunctionPrice("search-alpha")
		require.NoError(t, err, "改价后快照即时生效")
		require.Equal(t, int64(500), *got.PricePerCall)

		// codex-search 价可管理端改（种子行被接管语义一致）
		_, err = svc.UpsertManualFunctionPrice(ctx, functionManualReq(domain.CodexSearchModel, int64Ptr(2000)))
		require.NoError(t, err)
		got, err = svc.GetFunctionPrice(domain.CodexSearchModel)
		require.NoError(t, err)
		require.Equal(t, int64(2000), *got.PricePerCall, "codex-search 管理端改价即时生效")
	})
}

// TestDeleteManualFunctionPrice 删除手动按单元价：litellm 行 → ErrConflict；
// 成功后快照消失（GetFunctionPrice → ErrNotFound/默认兜底）。
func TestDeleteManualFunctionPrice(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFunctionFromLiteLLM(context.Background(), []*domain.FunctionPrice{
		functionLitellmRow("litellm-m", 10),
	})
	require.NoError(t, err)
	_, err = fs.UpsertFunctionManual(context.Background(), functionManualReq("manual-m", int64Ptr(20)))
	require.NoError(t, err)
	_, err = fs.UpsertFunctionManual(context.Background(), functionManualReq(domain.CodexSearchModel, int64Ptr(1000)))
	require.NoError(t, err)
	svc := newFunctionPricingSvc(t, fs)
	ctx := context.Background()

	err = svc.DeleteManualFunctionPrice(ctx, "litellm-m")
	require.ErrorIs(t, err, ErrConflict, "litellm 行不可删")

	require.NoError(t, svc.DeleteManualFunctionPrice(ctx, "manual-m"))
	_, err = svc.GetFunctionPrice("manual-m")
	require.ErrorIs(t, err, ErrNotFound, "删除后快照消失（下轮拉取补回）")

	// 删除 codex-search 种子行 → 快照消失 → 默认兜底生效（不中断计费）
	require.NoError(t, svc.DeleteManualFunctionPrice(ctx, domain.CodexSearchModel))
	p, err := svc.GetFunctionPrice(domain.CodexSearchModel)
	require.NoError(t, err, "codex-search 删除后默认兜底")
	require.Equal(t, domain.DefaultCodexSearchPricePerCall, *p.PricePerCall)
}

// TestFunctionPriceSnapshotFailSafe ListFunctionPrice 失败（functionListErr
// 注入）：ReloadFunctionPricingCtx 错误上报（注册表 Status 可观测）+
// reloadFunctionPricing Warn 路径保留旧快照（读路径仍命中旧行）；无旧快照 →
// 空快照读 codex-search 默认兜底而非 panic（对齐 TestImagePriceSnapshotFailSafe）。
func TestFunctionPriceSnapshotFailSafe(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFunctionFromLiteLLM(context.Background(), []*domain.FunctionPrice{
		functionLitellmRow("search-alpha", 10),
	})
	require.NoError(t, err)
	svc := newFunctionPricingSvc(t, fs)

	fs.functionListErr = errors.New("db down")
	require.Error(t, svc.ReloadFunctionPricingCtx(context.Background()), "加载失败错误上报（注册表 Status 可观测）")

	p, err := svc.GetFunctionPrice("search-alpha")
	require.NoError(t, err, "加载失败保留旧快照，读路径仍命中旧行")
	require.Equal(t, int64(10), *p.PricePerCall)

	// 无旧快照路径：reloadFunctionPricing（Warn 版）失败后空快照 →
	// codex-search 默认兜底、其他 ErrNotFound，而非 panic
	fs2 := newFakeStore()
	fs2.functionListErr = errors.New("db down")
	svc2 := New(fs2, nil, NopInvalidator{}, nil, nil, nil, nil)
	svc2.ReloadFunctionPricing() // 失败仅 Warn（log nil 时跳过），不返回错误
	p, err = svc2.GetFunctionPrice(domain.CodexSearchModel)
	require.NoError(t, err, "空快照 + codex-search → 默认兜底（防御语义）")
	require.Equal(t, domain.DefaultCodexSearchPricePerCall, *p.PricePerCall)
	_, err = svc2.GetFunctionPrice("any")
	require.ErrorIs(t, err, ErrNotFound, "空快照 + 其他 → ErrNotFound 而非 panic")
}

// TestListFunctionPrice 列表：source 筛选 + 非法 sort/source → ErrInvalidInput。
func TestListFunctionPrice(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFunctionFromLiteLLM(context.Background(), []*domain.FunctionPrice{
		functionLitellmRow("search-alpha", 10),
	})
	require.NoError(t, err)
	_, err = fs.UpsertFunctionManual(context.Background(), functionManualReq(domain.CodexSearchModel, int64Ptr(1000)))
	require.NoError(t, err)
	svc := newFunctionPricingSvc(t, fs)
	ctx := context.Background()

	rows, total, err := svc.ListFunctionPrice(ctx, repository.ListQuery{}, nil, "", "")
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, rows, 2)

	src := domain.PricingSourceManual
	rows, total, err = svc.ListFunctionPrice(ctx, repository.ListQuery{}, &src, "", "")
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, domain.PricingSourceManual, rows[0].Source)

	badSrc := domain.PricingSource("bogus")
	_, _, err = svc.ListFunctionPrice(ctx, repository.ListQuery{}, &badSrc, "", "")
	require.ErrorIs(t, err, ErrInvalidInput, "非法 source → 400")

	_, _, err = svc.ListFunctionPrice(ctx, repository.ListQuery{Sort: "bogus"}, nil, "", "")
	require.ErrorIs(t, err, ErrInvalidInput, "非法 sort → 400")
}

// TestFunctionPriceRow 管理端单行查询（DB 直读）：命中返回；缺失 → ErrNotFound
// （快照读的 codex-search 默认兜底不在此面出现）。
func TestFunctionPriceRow(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFunctionFromLiteLLM(context.Background(), []*domain.FunctionPrice{
		functionLitellmRow("search-alpha", 10),
	})
	require.NoError(t, err)
	svc := newFunctionPricingSvc(t, fs)
	ctx := context.Background()

	p, err := svc.GetFunctionPriceRow(ctx, "search-alpha")
	require.NoError(t, err)
	require.Equal(t, int64(10), *p.PricePerCall)

	_, err = svc.GetFunctionPriceRow(ctx, domain.CodexSearchModel)
	require.ErrorIs(t, err, ErrNotFound, "表行缺失 → 404（无兜底行）")
	_, err = svc.GetFunctionPriceRow(ctx, "missing")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestSyncPricingNowFunctionLine 手动 sync 三线：function 行独立落库 + function
// 快照重载 + 统计返回（FunctionRows/FunctionUpdated）。
func TestSyncPricingNowFunctionLine(t *testing.T) {
	fs := newFakeStore()
	svc := newFunctionPricingSvc(t, fs)
	svc.SetPriceFetcher(&fakePriceFetcher{res: &pricing.FetchResult{
		Rows:         []*domain.Pricing{litellmRow("gpt-4o", 250000, 1000000)},
		FunctionRows: []*domain.FunctionPrice{functionLitellmRow("search-alpha", 10)},
	}})

	stats, err := svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Rows)
	require.Equal(t, 1, stats.FunctionRows)
	require.Equal(t, 1, stats.FunctionUpdated)

	p, err := svc.GetFunctionPrice("search-alpha")
	require.NoError(t, err, "sync 后 function 快照已重载")
	require.Equal(t, int64(10), *p.PricePerCall)
	require.Equal(t, domain.PricingSourceLitellm, p.Source)

	// 三线统计并存（含 image 线）
	stats, err = svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, stats.ImageRows)
	require.Equal(t, 1, stats.FunctionRows, "重复 sync 幂等（manual 行级互斥下仍计落库数）")
}
