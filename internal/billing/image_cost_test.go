// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// imagePrice 构造 ImagePrice 测试行（三分量指针）。
func imagePrice(in, out, perImage *int64) *domain.ImagePrice {
	return &domain.ImagePrice{
		Model:                           "m",
		InputImageTokenPricePerMillion:  in,
		OutputImageTokenPricePerMillion: out,
		OutputCostPerImageMilli:         perImage,
	}
}

func i64(v int64) *int64 { return &v }

// TestImageCostTokenComponents gpt-image-2 官方形态换算断言（litellm 实参：
// input 8e-06 USD/token → 800,000 毫分/1M；output 3e-05 → 3,000,000）：
// token 分量走 /1e6 四舍五入；per-image 分量直接乘加。
func TestImageCostTokenComponents(t *testing.T) {
	p := imagePrice(i64(800000), i64(3000000), nil)

	// 1M input + 1M output tokens → (8e5×1e6+5e5)/1e6 = 800000.5 → 800000
	//                       + (3e6×1e6+5e5)/1e6 = 3000000.5 → 3000000
	require.Equal(t, int64(3800000), ImageCost(p, 1_000_000, 1_000_000, 0))

	// 0.5M input → (8e5×5e5+5e5)/1e6 = 400000.5 → 400000（四舍五入）
	require.Equal(t, int64(400000), ImageCost(p, 500_000, 0, 0))

	// 0 token → 0 成本
	require.Zero(t, ImageCost(p, 0, 0, 0))
}

// TestImageCostPerImage aiml 形态（0.054 USD/张 → 5,400 毫分/张）：per-image
// 直接乘 count，不走 /1e6 除法（5,400 而非 5400/1e6=0）。
func TestImageCostPerImage(t *testing.T) {
	p := imagePrice(nil, nil, i64(5400))

	require.Equal(t, int64(5400), ImageCost(p, 0, 0, 1), "1 张 → 5400 毫分（per-image 不走 /1e6）")
	require.Equal(t, int64(10800), ImageCost(p, 0, 0, 2), "2 张 → 10800")
	require.Zero(t, ImageCost(p, 0, 0, 0), "0 张 → 0")

	// 与 token 分量相加：1 张 + 1M input tokens
	p2 := imagePrice(i64(800000), nil, i64(5400))
	require.Equal(t, int64(800000+5400), ImageCost(p2, 1_000_000, 0, 1))
}

// TestImageCostNilComponents nil 分量 = 0（行存在但有效分量全 0 → cost = 0，
// 对齐 chat 0 价行语义）；nil 行（防御）→ 0。
func TestImageCostNilComponents(t *testing.T) {
	p := imagePrice(i64(800000), nil, nil)
	require.Equal(t, int64(800000), ImageCost(p, 1_000_000, 0, 0), "output 分量 nil → 0")
	require.Zero(t, ImageCost(p, 0, 1_000_000, 0), "output 分量 nil → 0（1M output tokens 也不计）")

	empty := imagePrice(nil, nil, nil)
	require.Zero(t, ImageCost(empty, 1_000_000, 1_000_000, 100), "全 nil 行 → 0")
	require.Zero(t, ImageCost(nil, 1_000_000, 1_000_000, 100), "nil 行（防御）→ 0")

	// 0 价行 → 0
	zero := imagePrice(i64(0), i64(0), i64(0))
	require.Zero(t, ImageCost(zero, math.MaxInt64, math.MaxInt64, math.MaxInt64), "0 价行 → 0")
}

// TestImageCostClamp count 钳制（评审 D1 + P2-4）：count 钳 (MaxInt64/1e5)/p，
// 钳后输出 ≤ MaxInt64/1e5；返回前总和再钳 MaxInt64/1e5（P2-4b）——
// 下游 applyMultiplier ×1e5 后恒不回绕（断言 ×1e5 为正且 ≤ MaxInt64）。
func TestImageCostClamp(t *testing.T) {
	const cap = int64(math.MaxInt64 / 100_000)

	t.Run("count clamp prevents per-image overflow", func(t *testing.T) {
		// perP=1：count 钳到 cap（MaxInt64 恶意 count → cap）
		p := imagePrice(nil, nil, i64(1))
		cost := ImageCost(p, 0, 0, math.MaxInt64)
		require.Equal(t, cap, cost, "count 钳到 (MaxInt64/1e5)/p")
		require.Greater(t, cost*100_000, int64(0), "×1e5 不回绕")
		require.LessOrEqual(t, cost*100_000, int64(math.MaxInt64), "×1e5 不回绕")

		// perP=2：count 钳到 cap/2，乘积 = 2×(cap/2) = cap-1（cap 奇数）≤ cap
		p2 := imagePrice(nil, nil, i64(2))
		cost2 := ImageCost(p2, 0, 0, math.MaxInt64)
		require.Equal(t, cap-1, cost2, "count 钳到 (MaxInt64/1e5)/p 后乘积 ≤ cap")
		require.LessOrEqual(t, cost2, cap)
		require.Greater(t, cost2*100_000, int64(0))
	})

	t.Run("total clamp covers token headroom (P2-4b)", func(t *testing.T) {
		// 最坏总和：perImage = cap - 1（count 钳后）+ tokenCost ~1.15e12 →
		// 超 cap → 返回前总和钳到 cap；×1e5 恒不回绕
		p := imagePrice(i64(1), nil, i64(2))
		cost := ImageCost(p, math.MaxInt64, 0, math.MaxInt64)
		require.Equal(t, cap, cost, "总和钳到 MaxInt64/1e5")
		require.Greater(t, cost*100_000, int64(0), "×1e5 不回绕")
		require.LessOrEqual(t, cost*100_000, int64(math.MaxInt64))
	})

	t.Run("token clamp same as chat clampToken", func(t *testing.T) {
		// 恶意 token 数：clampToken 钳到 segBudget/p → tokenCost ≤ segBudget/1e6
		p := imagePrice(i64(1), nil, nil)
		cost := ImageCost(p, math.MaxInt64, 0, 0)
		require.Positive(t, cost)
		require.LessOrEqual(t, cost, int64(segBudget)/1_000_000+1)
	})

	t.Run("negative inputs clamped to zero", func(t *testing.T) {
		p := imagePrice(i64(800000), i64(3000000), i64(5400))
		require.Zero(t, ImageCost(p, -1, -1, -1), "负 token/count → 0")
		require.Equal(t, int64(5400), ImageCost(p, -1, -1, 1), "count 钳 0 不影响正 count")
	})
}
