package service

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
)

// newPricingSvc 构造 Service（New 全路径：settings + pricing 快照初始化）。
func newPricingSvc(t *testing.T, fs *fakeStore) *Service {
	t.Helper()
	return New(fs, nil, func() {}, nil, nil, nil)
}

func litellmRow(model string, prompt, completion int64) *domain.Pricing {
	return &domain.Pricing{
		Model: model, PromptPricePerMillion: prompt,
		CompletionPricePerMillion: completion, Source: domain.PricingSourceLitellm,
	}
}

// TestPricingSnapshotLoadNew New 初始化从 DB 全量加载：GetPrice 快照命中
// （litellm + manual 行），缺失 → ErrNotFound（计费拒绝而非按 0 计价）。
func TestPricingSnapshotLoadNew(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{
		litellmRow("gpt-4o", 250000, 1000000),
		litellmRow("claude-3-5-sonnet", 300000, 1500000),
	})
	require.NoError(t, err)
	_, err = fs.UpsertManual(context.Background(), "gpt-4o-mini", 100, 200)
	require.NoError(t, err)

	svc := newPricingSvc(t, fs)

	p, err := svc.GetPrice("gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(250000), p.PromptPricePerMillion)
	require.Equal(t, domain.PricingSourceLitellm, p.Source)

	p, err = svc.GetPrice("gpt-4o-mini")
	require.NoError(t, err)
	require.Equal(t, int64(100), p.PromptPricePerMillion)
	require.Equal(t, domain.PricingSourceManual, p.Source)

	_, err = svc.GetPrice("no-such-model")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestPricingSnapshotReload ReloadPricing 后读路径即时生效（零 DB）。
func TestPricingSnapshotReload(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{litellmRow("m-a", 1, 2)})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)

	// 库内新增行：快照未刷新前读不到
	_, err = fs.UpsertManual(context.Background(), "m-b", 3, 4)
	require.NoError(t, err)
	_, err = svc.GetPrice("m-b")
	require.ErrorIs(t, err, ErrNotFound, "快照未刷新（DB 新增不自动可见）")

	svc.ReloadPricing()
	p, err := svc.GetPrice("m-b")
	require.NoError(t, err)
	require.Equal(t, int64(3), p.PromptPricePerMillion)
}

// TestUpsertManualPricing 管理端手动设价：校验（model 空/价格负数 → 400 语义）
// + 成功后自动重载快照（读路径即时生效，无需手动 ReloadPricing）。
func TestUpsertManualPricing(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{litellmRow("gpt-4o", 10, 20)})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	ctx := context.Background()

	t.Run("validation", func(t *testing.T) {
		_, err := svc.UpsertManualPricing(ctx, "", 1, 2)
		require.ErrorIs(t, err, ErrInvalidInput, "model 空 → 400")
		_, err = svc.UpsertManualPricing(ctx, "m", -1, 2)
		require.ErrorIs(t, err, ErrInvalidInput, "负价 → 400")
		_, err = svc.UpsertManualPricing(ctx, "m", 1, -2)
		require.ErrorIs(t, err, ErrInvalidInput)
	})

	t.Run("takeover litellm row and reload snapshot", func(t *testing.T) {
		p, err := svc.UpsertManualPricing(ctx, "gpt-4o", 999, 888)
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source)
		got, err := svc.GetPrice("gpt-4o")
		require.NoError(t, err)
		require.Equal(t, int64(999), got.PromptPricePerMillion, "接管 litellm 行且快照立即生效")
		require.Equal(t, domain.PricingSourceManual, got.Source)
	})
}

// TestDeleteManualPricing 管理端删手动价：成功后快照同步（该 model 从快照消失）；
// 失败（litellm 行 → ErrConflict / 缺失 → ErrNotFound）不刷新快照（原行保留）。
func TestDeleteManualPricing(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{litellmRow("m-litellm", 1, 2)})
	require.NoError(t, err)
	_, err = fs.UpsertManual(context.Background(), "m-manual", 3, 4)
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	ctx := context.Background()

	// litellm 行 → ErrConflict（只允许删手动价），快照保留
	err = svc.DeleteManualPricing(ctx, "m-litellm")
	require.ErrorIs(t, err, ErrConflict)
	_, err = svc.GetPrice("m-litellm")
	require.NoError(t, err, "删除失败快照保留")

	// 缺失 → ErrNotFound
	err = svc.DeleteManualPricing(ctx, "m-absent")
	require.ErrorIs(t, err, ErrNotFound)

	// 手动行删除成功 → 快照同步移除
	require.NoError(t, svc.DeleteManualPricing(ctx, "m-manual"))
	_, err = svc.GetPrice("m-manual")
	require.ErrorIs(t, err, ErrNotFound, "删手动价后快照消失（缺失窗口 GetPrice → ErrNotFound）")
}

// TestBuildPricingSnapshotManualPriority 快照构建的 manual > litellm 优先级：
// 同一 model 多行（防御；DB 唯一约束下不应出现）按 source 收敛，manual 恒胜。
func TestBuildPricingSnapshotManualPriority(t *testing.T) {
	t.Run("litellm first, manual later", func(t *testing.T) {
		m := buildPricingSnapshot([]*domain.Pricing{
			litellmRow("m", 1, 2),
			{Model: "m", PromptPricePerMillion: 9, CompletionPricePerMillion: 9, Source: domain.PricingSourceManual},
		})
		require.Equal(t, domain.PricingSourceManual, m["m"].Source, "manual 覆盖 litellm")
		require.Equal(t, int64(9), m["m"].PromptPricePerMillion)
	})

	t.Run("manual first, litellm later", func(t *testing.T) {
		m := buildPricingSnapshot([]*domain.Pricing{
			{Model: "m", PromptPricePerMillion: 9, CompletionPricePerMillion: 9, Source: domain.PricingSourceManual},
			litellmRow("m", 1, 2),
		})
		require.Equal(t, int64(9), m["m"].PromptPricePerMillion, "manual 行不被 litellm 覆盖")
		require.Equal(t, domain.PricingSourceManual, m["m"].Source)
	})

	t.Run("distinct models kept", func(t *testing.T) {
		m := buildPricingSnapshot([]*domain.Pricing{
			litellmRow("a", 1, 2), litellmRow("b", 3, 4),
		})
		require.Len(t, m, 2)
	})
}

// TestPricingSnapshotPaging 快照全量加载分页（> 1000 行跨多页取全）。
func TestPricingSnapshotPaging(t *testing.T) {
	fs := newFakeStore()
	rows := make([]*domain.Pricing, 0, 2500)
	for i := 0; i < 2500; i++ {
		rows = append(rows, litellmRow("pg-model-"+strconv.Itoa(i), int64(i), int64(i)))
	}
	_, err := fs.UpsertFromLiteLLM(context.Background(), rows)
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	for _, m := range []string{"pg-model-0", "pg-model-1000", "pg-model-2499"} {
		got, err := svc.GetPrice(m)
		require.NoError(t, err, "分页跨页行可达: %s", m)
		require.Equal(t, domain.PricingSourceLitellm, got.Source)
	}
}

// TestPricingSnapshotFailSafe ListPricing 失败 → Warn + 空快照（不阻断启动），
// GetPrice → ErrNotFound（而非 panic）。
func TestPricingSnapshotFailSafe(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{litellmRow("m", 1, 2)})
	require.NoError(t, err)
	fs.pricingListErr = errors.New("db down")
	svc := newPricingSvc(t, fs)
	_, err = svc.GetPrice("m")
	require.ErrorIs(t, err, ErrNotFound, "加载失败 → 空快照，读路径 ErrNotFound 而非 panic")
}
