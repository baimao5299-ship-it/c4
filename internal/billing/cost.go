// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package billing 计费核心：service_tier 归一化 + 价格矩阵纯函数计算。
// 纯函数零分配零锁；扣费落库（T3）与请求路径分离。
package billing

import (
	"math"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
)

// Tier service_tier 归一化档位。与 sub2api 的 fast→priority 归一不同：fast
// 为独立档位（Anthropic Fast Mode 整单倍率，Anthropic 官方语义；用户裁决，
// 见 Phase 5 计划）。
type Tier int

const (
	TierAuto Tier = iota // 默认档（auto/default/scale/空/未知，恒透传）
	TierFast             // Anthropic Fast Mode（fast_multiplier 整单倍率）
	TierPriority
	TierFlex
)

// String 归一化字符串（UsageLog.BillingTier 落库值）。
func (t Tier) String() string {
	switch t {
	case TierFast:
		return "fast"
	case TierPriority:
		return "priority"
	case TierFlex:
		return "flex"
	default:
		return "auto"
	}
}

// NormalizeTier 归一化请求 service_tier：小写去空格；fast/priority/flex 精确
// 匹配；空/auto/default/scale/未知 → TierAuto（默认档）。
func NormalizeTier(raw string) Tier {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "fast":
		return TierFast
	case "priority":
		return TierPriority
	case "flex":
		return TierFlex
	default:
		return TierAuto
	}
}

// TierPolicyMode service_tier 转发策略（settings 键 service_tier_policy_priority
// / service_tier_policy_flex / service_tier_policy_fast；缺失/未知值 →
// passthrough 默认）。auto/空恒透传（proxy 侧短路，不查策略）。
type TierPolicyMode string

const (
	TierPolicyPassthrough TierPolicyMode = "passthrough" // 原样转发（默认）
	TierPolicyStrip       TierPolicyMode = "strip"       // 转发体删除 service_tier 字段
	TierPolicyReject      TierPolicyMode = "reject"      // 直接 400 拒绝（不转发）
)

// 溢出预算（评审 I-2）：合法域单段 t×p ≤ 1e13（毫分×1e6 原始单位），四分量
// 合计 ≤ 4e13；fast 万分数 ≤ 1e5（实测 ×6.0 = 60000，留余量）→ 4e13×1e5
// = 4e18 < MaxInt64。fast 万分数上界由钳制强制（A-P2-4 双保险：本函数 fast
// 分支 + pricing fetch assign 同钳 > 1e5 → 1e5——快照可经任何途径进入超界
// 值含手动 DB 操作，钳后本预算先决条件恢复成立）。价格列已由写路径校验 ≥ 0
// （service 校验/fetcher validCost）；负数 token 钳 0。除 1e6 在求和后一次
// 完成（毫分/1M tokens → 毫分）。
const (
	milliPerMillion = 1_000_000 // 毫分/1M tokens → 毫分的除数
	// CostRemainderScale is the denominator used by CostParts.Remainder.
	// Exporting the scale lets the proxy aggregate fractional request costs
	// before applying a group multiplier without duplicating a magic number.
	CostRemainderScale int64 = milliPerMillion
)

// segBudget 单乘积上界（评审 I-1 溢出钳制）：每个 token 计费分量由
// saturatingCostMul 限制到此值，四分量求和后仍留出 round-half-up 的余量，
// 因此末次舍入不会回绕。异常高价格会得到这个正上限，不会因整除得到零。
const segBudget = (math.MaxInt64 - milliPerMillion/2) / 8

// saturatingCostMul multiplies two non-negative billing values while keeping
// the result within limit. Computing the quotient before multiplying avoids
// int64 wraparound. In particular, when b > limit, limit/b is zero; returning
// limit for any positive a preserves a positive debit instead of turning an
// unusually high price into a free request.
func saturatingCostMul(a, b, limit int64) int64 {
	if a <= 0 || b <= 0 || limit <= 0 {
		return 0
	}
	if a > limit/b {
		return limit
	}
	return a * b
}

const imageTotalCap = math.MaxInt64 / 100_000

// CostParts is the fixed-point result before rounding to the ledger unit.
// Units are whole milli-cents and Remainder is the numerator left after the
// one-million-token division. The proxy carries Remainder across requests so
// a very small configured price is not silently treated as free forever.
type CostParts struct {
	Units     int64
	Remainder int64
}

// Rounded returns the legacy standalone round-half-up result. The live proxy
// aggregates Remainder across requests, while pure callers retain the existing
// one-request contract through this method.
func (p CostParts) Rounded() int64 { return roundCostParts(p) }

func positiveCostPrice(v *int64) int64 {
	if v == nil || *v <= 0 {
		return 0
	}
	return *v
}

func splitCostParts(raw int64) CostParts {
	if raw <= 0 {
		return CostParts{}
	}
	return CostParts{Units: raw / milliPerMillion, Remainder: raw % milliPerMillion}
}

// CostPartsFromResolved returns token-priced usage without rounding away its
// fractional ledger unit. CostFromResolved remains the pure half-up API for
// callers that need a standalone integer result.
func CostPartsFromResolved(rp domain.ResolvedPrices, pt, ct, cr, cc int64) CostParts {
	clamp := func(t int64) int64 {
		if t < 0 {
			return 0
		}
		return t
	}
	pt, ct, cr, cc = clamp(pt), clamp(ct), clamp(cr), clamp(cc)
	in, out := positiveCostPrice(rp.InputPerM), positiveCostPrice(rp.OutputPerM)
	cacheRead, cacheWrite := positiveCostPrice(rp.CacheReadPerM), positiveCostPrice(rp.CacheWritePerM)
	// A provider may report cache usage while the catalogue only contains the
	// base input rate. Treat a missing cache rate as input-priced usage instead
	// of silently making that component free. An explicit zero pointer remains a
	// deliberate free price and is not replaced by the fallback.
	if rp.CacheReadPerM == nil {
		cacheRead = in
	}
	if rp.CacheWritePerM == nil {
		cacheWrite = in
	}
	raw := saturatingCostAdd(saturatingCostMul(pt, in, segBudget), saturatingCostMul(ct, out, segBudget))
	raw = saturatingCostAdd(raw, saturatingCostMul(cr, cacheRead, segBudget))
	raw = saturatingCostAdd(raw, saturatingCostMul(cc, cacheWrite, segBudget))
	return splitCostParts(raw)
}

// CostFromResolved pure arithmetic on resolved prices (new unified path).
func CostFromResolved(rp domain.ResolvedPrices, pt, ct, cr, cc int64) int64 {
	return CostPartsFromResolved(rp, pt, ct, cr, cc).Rounded()
}

// ImageCostFromResolved image branch via unified ResolvedPrices.
func ImageCostFromResolved(rp domain.ResolvedPrices, inTok, outTok, count int64) int64 {
	return ImageCostPartsFromResolved(rp, inTok, outTok, count).Rounded()
}

// ImageCostPartsFromResolved is the fixed-point image equivalent. The
// per-image component is already in ledger units, so only image-token prices
// contribute a remainder.
func ImageCostPartsFromResolved(rp domain.ResolvedPrices, inTok, outTok, count int64) CostParts {
	clamp := func(t int64) int64 {
		if t < 0 {
			return 0
		}
		return t
	}
	inTok, outTok, count = clamp(inTok), clamp(outTok), clamp(count)
	inP, outP, perP := positiveCostPrice(rp.ImgInTokPerM), positiveCostPrice(rp.ImgOutTokPerM), positiveCostPrice(rp.PricePerImage)
	raw := saturatingCostAdd(saturatingCostMul(inTok, inP, segBudget), saturatingCostMul(outTok, outP, segBudget))
	parts := splitCostParts(raw)
	if perP > 0 && count > 0 {
		parts.Units = saturatingCostAdd(parts.Units, saturatingCostMul(count, perP, imageTotalCap))
	}
	if parts.Units >= imageTotalCap {
		parts.Units = imageTotalCap
		parts.Remainder = 0
	}
	return parts
}

// AddCostParts combines fixed-point cost components without rounding between
// them. This is used when one response contains token usage plus a flat image
// charge, so the token fraction survives until the proxy's aggregate ledger
// rounding step.
func AddCostParts(a, b CostParts) CostParts {
	units := saturatingCostAdd(a.Units, b.Units)
	if units == math.MaxInt64 {
		return CostParts{Units: math.MaxInt64}
	}
	remainder := a.Remainder + b.Remainder
	carry := remainder / milliPerMillion
	if units > math.MaxInt64-carry {
		return CostParts{Units: math.MaxInt64}
	}
	return CostParts{Units: units + carry, Remainder: remainder % milliPerMillion}
}

// saturatingCostAdd combines provider-derived cost components without allowing
// a malformed price or usage value to wrap a positive debit into a credit.
func saturatingCostAdd(a, b int64) int64 {
	if a <= 0 {
		if b <= 0 {
			return 0
		}
		return b
	}
	if b <= 0 {
		return a
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// roundedCost applies the existing round-half-up policy without adding the
// half-unit to an int64 value that may already be at the upper bound.
func roundedCost(raw int64) int64 {
	return splitCostParts(raw).Rounded()
}

func roundCostParts(parts CostParts) int64 {
	units := parts.Units
	if units < 0 {
		units = 0
	}
	if parts.Remainder >= milliPerMillion/2 && units < math.MaxInt64 {
		units++
	}
	return units
}

// CallCostFromResolved per-call branch.
func CallCostFromResolved(rp domain.ResolvedPrices, count int64) int64 {
	if count < 0 {
		count = 0
	}
	if rp.PricePerCall == nil {
		return 0
	}
	if p := *rp.PricePerCall; p > 0 {
		return saturatingCostMul(count, p, imageTotalCap)
	}
	return 0
}
