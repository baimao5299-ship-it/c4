// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
)

// newImagePricingSvc 构造 Service + 显式首刷双线价格快照（文本价 + image 价，
// 对齐 newPricingSvc 语义——New 不自载，首刷由注册表 ReloadAll 承担）。
func newImagePricingSvc(t *testing.T, fs *fakeStore) *Service {
	t.Helper()
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	require.NoError(t, svc.ReloadImagePricingCtx(context.Background()))
	return svc
}

func imageLitellmRow(model string, in, out, perImage *int64) *domain.ImagePrice {
	return &domain.ImagePrice{
		Model:                           model,
		InputImageTokenPricePerMillion:  in,
		OutputImageTokenPricePerMillion: out,
		OutputCostPerImageMilli:         perImage,
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

// TestImagePriceSnapshotLoadNew 快照首载：GetImagePrice 命中（litellm + manual
// 行），缺失 → ErrNotFound（调用方 402 语义——拒绝计费而非按 0 计价）。
func TestImagePriceSnapshotLoadNew(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertImageFromLiteLLM(context.Background(), []*domain.ImagePrice{
		imageLitellmRow("gpt-image-2", int64Ptr(800000), int64Ptr(3000000), nil),
	})
	require.NoError(t, err)
	_, err = fs.UpsertImageManual(context.Background(), imageManualReq("aiml-image", nil, nil, int64Ptr(5400)))
	require.NoError(t, err)

	svc := newImagePricingSvc(t, fs)

	p, err := svc.GetImagePrice("gpt-image-2")
	require.NoError(t, err)
	require.Equal(t, int64(800000), *p.InputImageTokenPricePerMillion)
	require.Equal(t, domain.PricingSourceLitellm, p.Source)

	p, err = svc.GetImagePrice("aiml-image")
	require.NoError(t, err)
	require.Equal(t, int64(5400), *p.OutputCostPerImageMilli)
	require.Equal(t, domain.PricingSourceManual, p.Source)

	_, err = svc.GetImagePrice("no-such-model")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestImagePriceSnapshotReload ReloadImagePricing 后读路径即时生效（零 DB）。
func TestImagePriceSnapshotReload(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertImageFromLiteLLM(context.Background(), []*domain.ImagePrice{
		imageLitellmRow("gpt-image-2", int64Ptr(800000), nil, nil),
	})
	require.NoError(t, err)
	svc := newImagePricingSvc(t, fs)

	// 库内新增行：快照未刷新前读不到
	_, err = fs.UpsertImageManual(context.Background(), imageManualReq("m-b", int64Ptr(1), nil, nil))
	require.NoError(t, err)
	_, err = svc.GetImagePrice("m-b")
	require.ErrorIs(t, err, ErrNotFound, "快照未刷新（DB 新增不自动可见）")

	svc.ReloadImagePricing()
	p, err := svc.GetImagePrice("m-b")
	require.NoError(t, err)
	require.Equal(t, int64(1), *p.InputImageTokenPricePerMillion)
}

// TestUpsertManualImagePrice 管理端手动设图价格：校验（model 空 / 全 nil →
// 400；负数 → 400）+ 成功后自动重载快照（读路径即时生效）。
func TestUpsertManualImagePrice(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertImageFromLiteLLM(context.Background(), []*domain.ImagePrice{
		imageLitellmRow("gpt-image-2", int64Ptr(10), int64Ptr(20), nil),
	})
	require.NoError(t, err)
	svc := newImagePricingSvc(t, fs)
	ctx := context.Background()

	t.Run("validation", func(t *testing.T) {
		_, err := svc.UpsertManualImagePrice(ctx, imageManualReq("", int64Ptr(1), nil, nil))
		require.ErrorIs(t, err, ErrInvalidInput, "model 空 → 400")
		_, err = svc.UpsertManualImagePrice(ctx, imageManualReq("m", nil, nil, nil))
		require.ErrorIs(t, err, ErrInvalidInput, "三分量全 nil → 400（行有效性 = 至少一价）")
		_, err = svc.UpsertManualImagePrice(ctx, imageManualReq("m", int64Ptr(-1), nil, nil))
		require.ErrorIs(t, err, ErrInvalidInput, "负 token 价 → 400")
		_, err = svc.UpsertManualImagePrice(ctx, imageManualReq("m", nil, int64Ptr(0), nil))
		require.NoError(t, err, "显式 0 价 = 该价明确为 0，允许")
	})

	t.Run("success reloads snapshot", func(t *testing.T) {
		p, err := svc.UpsertManualImagePrice(ctx, imageManualReq("gpt-image-2", int64Ptr(800000), int64Ptr(3000000), int64Ptr(5400)))
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source, "接管 litellm 行")
		got, err := svc.GetImagePrice("gpt-image-2")
		require.NoError(t, err, "改价后快照即时生效")
		require.Equal(t, int64(800000), *got.InputImageTokenPricePerMillion)
		require.Equal(t, int64(5400), *got.OutputCostPerImageMilli)
	})
}

// TestDeleteManualImagePrice 删除手动图价格：litellm 行 → ErrConflict；
// 成功后快照消失（GetImagePrice → ErrNotFound）。
func TestDeleteManualImagePrice(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertImageFromLiteLLM(context.Background(), []*domain.ImagePrice{
		imageLitellmRow("litellm-m", int64Ptr(1), nil, nil),
	})
	require.NoError(t, err)
	_, err = fs.UpsertImageManual(context.Background(), imageManualReq("manual-m", int64Ptr(2), nil, nil))
	require.NoError(t, err)
	svc := newImagePricingSvc(t, fs)
	ctx := context.Background()

	err = svc.DeleteManualImagePrice(ctx, "litellm-m")
	require.ErrorIs(t, err, ErrConflict, "litellm 行不可删")

	require.NoError(t, svc.DeleteManualImagePrice(ctx, "manual-m"))
	_, err = svc.GetImagePrice("manual-m")
	require.ErrorIs(t, err, ErrNotFound, "删除后快照消失（下轮拉取补回）")
}

// TestListImagePrice 列表：source 筛选 + 非法 sort/source → ErrInvalidInput。
func TestListImagePrice(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertImageFromLiteLLM(context.Background(), []*domain.ImagePrice{
		imageLitellmRow("gpt-image-2", int64Ptr(800000), nil, nil),
	})
	require.NoError(t, err)
	_, err = fs.UpsertImageManual(context.Background(), imageManualReq("aiml-image", nil, nil, int64Ptr(5400)))
	require.NoError(t, err)
	svc := newImagePricingSvc(t, fs)
	ctx := context.Background()

	rows, total, err := svc.ListImagePrice(ctx, repository.ListQuery{}, nil, "")
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, rows, 2)

	src := domain.PricingSourceManual
	rows, total, err = svc.ListImagePrice(ctx, repository.ListQuery{}, &src, "")
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, domain.PricingSourceManual, rows[0].Source)

	badSrc := domain.PricingSource("bogus")
	_, _, err = svc.ListImagePrice(ctx, repository.ListQuery{}, &badSrc, "")
	require.ErrorIs(t, err, ErrInvalidInput, "非法 source → 400")

	_, _, err = svc.ListImagePrice(ctx, repository.ListQuery{Sort: "bogus"}, nil, "")
	require.ErrorIs(t, err, ErrInvalidInput, "非法 sort → 400")
}

// TestSyncPricingNowImageLine 手动 sync 双线：image 行独立落库 + image 快照
// 重载 + 统计返回（ImageRows/ImageUpdated）。
func TestSyncPricingNowImageLine(t *testing.T) {
	fs := newFakeStore()
	svc := newImagePricingSvc(t, fs)
	svc.SetPriceFetcher(&fakePriceFetcher{res: &pricing.FetchResult{
		Rows:      []*domain.Pricing{litellmRow("gpt-4o", 250000, 1000000)},
		ImageRows: []*domain.ImagePrice{imageLitellmRow("gpt-image-2", int64Ptr(800000), int64Ptr(3000000), nil)},
	}})

	stats, err := svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Rows)
	require.Equal(t, 1, stats.ImageRows)
	require.Equal(t, 1, stats.ImageUpdated)

	p, err := svc.GetImagePrice("gpt-image-2")
	require.NoError(t, err, "sync 后 image 快照已重载")
	require.Equal(t, int64(800000), *p.InputImageTokenPricePerMillion)
	require.Equal(t, domain.PricingSourceLitellm, p.Source)
}
