// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"math"

	"github.com/is7qin/c3api/internal/domain"
)

// imageTotalCap 返回前总和钳制上界（评审 P2-4b）：下游 applyMultiplier ×1e5
// （billing.go:56-58）后恒不回绕——钳制后 total ≤ MaxInt64/1e5，×1e5 ≤
// MaxInt64 - (MaxInt64 % 1e5)。count 钳（imageTotalCap/p）未给 token 分量留
// 余量（最坏总和 ~9.45e13 × 1e5 = 9.45e18 仍超 MaxInt64），故总和返回前再钳。
const imageTotalCap = math.MaxInt64 / 100_000

// ImageCost 生图计费纯函数（独立函数，不扩展 chat 的 Cost——文本模型无 image
// 分量，chat Cost 的 8 乘积溢出预算不动）：image token 分量（毫分/1M →
// 毫分，/1e6 四舍五入）+ per-image 分量（毫分/张 直接乘加——不走 /1e6！）。
//
// 单位（spec 表头定死）：1 USD = 100,000 毫分。
//   - InputImageTokenPricePerMillion / OutputImageTokenPricePerMillion：
//     毫分/1M image tokens（litellm gpt-image-2 官方形态 8e-06 → 800,000、
//     3e-05 → 3,000,000）
//   - OutputCostPerImageMilli：毫分/张（aiml 0.054 → 5,400）
//
// 溢出预算（评审 D1 + P2-4）：函数内 3 乘积（image input/output 各 1——无
// above/tier/fast 分段 + per-image 1）；下游 ×1e5 不变量对 per-image 不成立
// （chat 先除 1e6，per-image 不分除，输出可达 ~0.375×MaxInt64）——故：
//   - token 分量钳制同 clampToken（恶意 token 数防回绕）
//   - count 钳制上限取 (MaxInt64/1e5)/p（而非 segBudget/p），钳后输出
//     ≤ MaxInt64/1e5 量级
//   - 返回前总和再钳 MaxInt64/1e5（P2-4b）
//
// fast/组倍率整单作用于含 image 分量的 cost（与 chat 同路径，声明——调用方
// 下游 applyMultiplier 统一施加，本函数不含倍率）。nil 分量 = 0；行存在但
// 有效分量全 0 → cost = 0（对齐 chat 0 价行语义）。负 token/count 钳 0
// （恶意/异常响应防护）。零分配零锁（热路径）。
func ImageCost(p *domain.ImagePrice, imageInputTokens, imageOutputTokens, imageCount int64) int64 {
	clamp := func(t int64) int64 {
		if t < 0 {
			return 0
		}
		return t
	}
	in, out, count := clamp(imageInputTokens), clamp(imageOutputTokens), clamp(imageCount)
	var inP, outP, perP int64
	if p != nil {
		if p.InputImageTokenPricePerMillion != nil {
			inP = *p.InputImageTokenPricePerMillion
		}
		if p.OutputImageTokenPricePerMillion != nil {
			outP = *p.OutputImageTokenPricePerMillion
		}
		if p.OutputCostPerImageMilli != nil {
			perP = *p.OutputCostPerImageMilli
		}
	}
	raw := clampToken(in, inP)*inP + clampToken(out, outP)*outP // 钳制后单乘积 ≤ segBudget
	tokenCost := (raw + milliPerMillion/2) / milliPerMillion    // 仅 token 分量走 /1e6
	var perImage int64
	if perP > 0 && count > 0 {
		// count 钳：官方 n 上限 10，但恶意/异常响应可报任意 data 长——钳到
		// (MaxInt64/1e5)/p（评审 P2-4：覆盖下游 ×1e5 倍率的不变量）。
		if lim := imageTotalCap / perP; count > lim {
			count = lim
		}
		perImage = count * perP // 每张价直接乘，不走 /1e6！
	}
	total := tokenCost + perImage
	if total > imageTotalCap {
		total = imageTotalCap // 返回前总和钳制（P2-4b：count 钳未给 token 分量留余量）
	}
	return total
}
