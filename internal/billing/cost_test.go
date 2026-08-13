// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// fixturePricing 构建对齐 litellm 官方表实测形态的价格行（fetch_test.go
// matrixFixtureJSON 同源数值）：gpt-5.6-sol 全矩阵（priority/flex 单价档 +
// above_flex 组 4 分量 + fast ×2.0）、azure/gpt-5.6-sol（above_priority 组
// 无 cache_creation + fast ×6.0）、future-256k（above 基础组）。
func fixturePricing(model string) *domain.Pricing {
	i64 := func(v int64) *int64 { return &v }
	switch model {
	case "gpt-5.6-sol":
		return &domain.Pricing{
			Model:                                 model,
			PromptPricePerMillion:                 200000, // 2e-6 USD/token × 1e11
			CompletionPricePerMillion:             800000,
			PriorityPromptPricePerMillion:         i64(300000),
			PriorityCompletionPricePerMillion:     i64(1200000),
			PriorityCacheReadPricePerMillion:      i64(100000),
			PriorityCacheCreationPricePerMillion:  i64(200000),
			FlexPromptPricePerMillion:             i64(150000),
			FlexCompletionPricePerMillion:         i64(600000),
			FlexCacheReadPricePerMillion:          i64(50000),
			FlexCacheCreationPricePerMillion:      i64(100000),
			AboveThreshold:                        i64(272000),
			AboveFlexPromptPricePerMillion:        i64(120000),
			AboveFlexCompletionPricePerMillion:    i64(480000),
			AboveFlexCacheReadPricePerMillion:     i64(40000),
			AboveFlexCacheCreationPricePerMillion: i64(80000),
			FastMultiplier:                        i64(20000), // ×2.0
		}
	case "azure/gpt-5.6-sol":
		return &domain.Pricing{
			Model:                                     model,
			PromptPricePerMillion:                     200000,
			CompletionPricePerMillion:                 800000,
			PriorityPromptPricePerMillion:             i64(300000),
			PriorityCompletionPricePerMillion:         i64(1200000),
			AboveThreshold:                            i64(272000),
			AbovePriorityPromptPricePerMillion:        i64(180000),
			AbovePriorityCompletionPricePerMillion:    i64(720000),
			AbovePriorityCacheReadPricePerMillion:     i64(60000),
			AbovePriorityCacheCreationPricePerMillion: nil,        // azure 实测形态：组内缺失
			FastMultiplier:                            i64(60000), // ×6.0
		}
	case "future-256k":
		return &domain.Pricing{
			Model:                          model,
			PromptPricePerMillion:          100000,
			CompletionPricePerMillion:      200000,
			CacheReadPricePerMillion:       i64(30000),
			AboveThreshold:                 i64(256000),
			AbovePromptPricePerMillion:     i64(90000),
			AboveCompletionPricePerMillion: i64(180000),
			AboveCacheReadPricePerMillion:  i64(30000),
		}
	}
	panic("unknown fixture model: " + model)
}

// TestNormalizeTier 全分支：大小写/空白归一；fast/priority/flex 精确匹配；
// 空/auto/default/scale/未知 → TierAuto。
func TestNormalizeTier(t *testing.T) {
	cases := []struct {
		raw  string
		want Tier
	}{
		{"", TierAuto},
		{"auto", TierAuto},
		{"default", TierAuto},
		{"scale", TierAuto},
		{"unknown", TierAuto},
		{"fast", TierFast},
		{" Fast ", TierFast},
		{"priority", TierPriority},
		{"PRIORITY", TierPriority},
		{"flex", TierFlex},
		{"Flex", TierFlex},
	}
	for _, c := range cases {
		require.Equal(t, c.want, NormalizeTier(c.raw), "NormalizeTier(%q)", c.raw)
	}
	require.Equal(t, "auto", TierAuto.String())
	require.Equal(t, "fast", TierFast.String())
	require.Equal(t, "priority", TierPriority.String())
	require.Equal(t, "flex", TierFlex.String())
}

// TestCost litellm 真实值表驱动：选价组合/above 分段/fast 倍率/边界/钳零/溢出。
func TestCost(t *testing.T) {
	cases := []struct {
		name           string
		model          string
		tier           Tier
		pt, ct, cr, cc int64
		wantCost       int64
		wantHit        bool
	}{
		{
			name:  "auto 基础价（无倍率无分段）",
			model: "gpt-5.6-sol", tier: TierAuto,
			pt: 100000, ct: 50000,
			wantCost: 60000, wantHit: false, // 2e10+4e10 = 6e10 → 60000
		},
		{
			name:  "flex + above_flex 全矩阵分段（4 分量）",
			model: "gpt-5.6-sol", tier: TierFlex,
			pt: 300000, ct: 300000, cr: 100000,
			wantCost: 225800, wantHit: true, // pt/ct 拆段，cr ≤ 阈值不拆
		},
		{
			name:  "priority + above_priority 组内缺失分量不拆段（azure）",
			model: "azure/gpt-5.6-sol", tier: TierPriority,
			pt: 300000, ct: 300000, cr: 300000, cc: 100000,
			wantCost: 434880, wantHit: true, // cc 无 above 价 → 全按组内价（无基础缓存价 → 0）
		},
		{
			name:  "priority 单价档 + above 组缺失不拆段",
			model: "gpt-5.6-sol", tier: TierPriority,
			pt: 300000, ct: 100000,
			wantCost: 210000, wantHit: false, // t > 阈值但 above_priority 缺失 → 不拆段
		},
		{
			name:  "fast ×2.0 整单倍率",
			model: "gpt-5.6-sol", tier: TierFast,
			pt: 100000, ct: 50000,
			wantCost: 120000, wantHit: false, // 60000×2.0
		},
		{
			name:  "fast ×6.0 整单倍率",
			model: "azure/gpt-5.6-sol", tier: TierFast,
			pt: 100000, ct: 50000,
			wantCost: 360000, wantHit: false, // 60000×6.0
		},
		{
			name:  "分段边界 == 阈值不分段",
			model: "gpt-5.6-sol", tier: TierFlex,
			pt: 272000, ct: 272000,
			wantCost: 204000, wantHit: false, // 40800+163200；t == thr 不拆
		},
		{
			name:  "无 tier 价回退基础（不涨价）",
			model: "future-256k", tier: TierPriority,
			pt: 200000, ct: 100000,
			wantCost: 40000, wantHit: false, // 与 auto 同价
		},
		{
			name:  "auto + above 基础组分段",
			model: "future-256k", tier: TierAuto,
			pt: 300000, ct: 300000, cr: 50000,
			wantCost: 90180, wantHit: true, // 29560+59120+1500
		},
		{
			name:  "负数 token 钳 0",
			model: "gpt-5.6-sol", tier: TierAuto,
			pt: -100, ct: 1000,
			wantCost: 800, wantHit: false, // pt 钳 0，仅 ct
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cost, hit := Cost(fixturePricing(c.model), c.tier, c.pt, c.ct, c.cr, c.cc)
			require.Equal(t, c.wantCost, cost)
			require.Equal(t, c.wantHit, hit)
		})
	}
}

// TestCostNoAboveThreshold 阈值缺失（manual 行只设 above 价不设阈值）→ 不拆段，
// above 价不参与计算。
func TestCostNoAboveThreshold(t *testing.T) {
	p := fixturePricing("future-256k")
	p.AboveThreshold = nil
	cost, hit := Cost(p, TierAuto, 300000, 0, 0, 0)
	require.Equal(t, int64(30000), cost, "无阈值 → 全按基础价")
	require.False(t, hit)
}

// TestCostNoCachePrice 无缓存价分量 → 0（不发明价格）。
func TestCostNoCachePrice(t *testing.T) {
	p := fixturePricing("future-256k") // 无 cache_creation 价
	cost, hit := Cost(p, TierAuto, 1000, 1000, 1000, 1000)
	require.Equal(t, int64(330), cost, "cc 分量无价 → 0（100+200+30）")
	require.False(t, hit)
}

// TestCostOverflowBudget 溢出防御（评审 I-2 预算极限）：单段 t×p = 1e13、
// 四分量合计 4e13（毫分×1e6 原始单位），fast ×1e5 → 4e18 < MaxInt64，无回绕。
func TestCostOverflowBudget(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	p := &domain.Pricing{
		PromptPricePerMillion:        1e11, // $1000/1M 极端价
		CompletionPricePerMillion:    1e11,
		CacheReadPricePerMillion:     i64(1e11),
		CacheCreationPricePerMillion: i64(1e11),
		FastMultiplier:               i64(100000), // ×10.0 上限
	}
	cost, hit := Cost(p, TierAuto, 100, 100, 100, 100)
	require.Equal(t, int64(4e7), cost, "4×1e13 → 4e7 毫分，无溢出")
	require.False(t, hit)
	cost, hit = Cost(p, TierFast, 100, 100, 100, 100)
	require.Equal(t, int64(4e8), cost, "fast ×1e5：4e7×1e5 = 4e12，无溢出")
	require.False(t, hit)

	// A-P2-4 超界 m（仅可经手动 DB 操作或异常源进入，钳制而非拒绝）：fast
	// 分支钳到 1e5 后结果与上限档一致，无回绕——钳前 4e7×1e9 = 4e16 超收
	// 1e4 倍、4e7×1e15 = 4e22 溢出 MaxInt64 回绕成负。
	p.FastMultiplier = i64(1e9)
	cost, hit = Cost(p, TierFast, 100, 100, 100, 100)
	require.Equal(t, int64(4e8), cost, "m=1e9 钳到 1e5：4e7×1e5 = 4e12，无超收")
	require.False(t, hit)
	p.FastMultiplier = i64(1e15)
	cost, hit = Cost(p, TierFast, 100, 100, 100, 100)
	require.Equal(t, int64(4e8), cost, "m=1e15 钳到 1e5：无回绕（钳前 4e22 溢出 MaxInt64）")
	require.False(t, hit)
}

// TestCostTokenClamp 溢出钳制（评审 I-1）：恶意/异常上游报超大 token（9e15/
// 1e13 量级）时乘法前钳制，防 int64 回绕成负 cost（负 cost 会让 T3 扣费变
// 反向入账）。钳制后 cost 恒 ≥ 0；正常值结果不变（TestCost 表驱动已钉死）。
func TestCostTokenClamp(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	// 极端价 1e11/1M（同 TestCostOverflowBudget）：9e15 token 每分量钳到
	// segBudget/1e11 = 11529215 → 乘积 1.1529215e18，4 分量合计 4.611686e18
	// < MaxInt64，无回绕。
	extreme := &domain.Pricing{
		PromptPricePerMillion:        1e11,
		CompletionPricePerMillion:    1e11,
		CacheReadPricePerMillion:     i64(1e11),
		CacheCreationPricePerMillion: i64(1e11),
	}
	cost, hit := Cost(extreme, TierAuto, 9e15, 9e15, 9e15, 9e15)
	require.Equal(t, int64(4611686000000), cost, "4×1.1529215e18 → 4.611686e12 毫分，无回绕")
	require.False(t, hit)

	// 8 乘积极端：above 价 2e11 更高，全分量拆段（thr 272000×基础价 +
	// 超额钳到 segBudget/2e11 = 5764607 × above 价）。
	extremeAbove := &domain.Pricing{
		PromptPricePerMillion:             1e11,
		CompletionPricePerMillion:         1e11,
		CacheReadPricePerMillion:          i64(1e11),
		CacheCreationPricePerMillion:      i64(1e11),
		AboveThreshold:                    i64(272000),
		AbovePromptPricePerMillion:        i64(2e11),
		AboveCompletionPricePerMillion:    i64(2e11),
		AboveCacheReadPricePerMillion:     i64(2e11),
		AboveCacheCreationPricePerMillion: i64(2e11),
	}
	cost, hit = Cost(extremeAbove, TierAuto, 9e15, 9e15, 9e15, 9e15)
	require.Equal(t, int64(4720485600000), cost, "4×(2.72e16+1.1529214e18) → 4.7204856e12 毫分")
	require.True(t, hit)

	// 1e13 量级：钳制后 cost ≥ 0 且不回绕。
	cost, hit = Cost(extreme, TierAuto, 1e13, 0, 0, 0)
	require.Equal(t, int64(1152921500000), cost, "钳到 11529215×1e11 → 1.1529215e12 毫分")
	require.False(t, hit)

	// 1e13 token × 正常价 1e5（乘积 1e18 本就不溢出）：钳制不上界 → 结果不变。
	p := fixturePricing("future-256k")
	p.AboveThreshold = nil // 去掉 above 分段，纯基础价
	cost, hit = Cost(p, TierAuto, 1e13, 0, 0, 0)
	require.Equal(t, int64(1000000000000), cost, "1e13×1e5 = 1e18 → 1e12 毫分")
	require.False(t, hit)
}
