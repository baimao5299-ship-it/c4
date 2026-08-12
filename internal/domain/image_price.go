// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"encoding/json"
	"time"
)

// ImagePrice 图片生成价格行（images 端点计费价格来源；一行 = 最终生效价，
// 计费读快照不关心来源）。价格单位（与 pricings 表头同款 1 USD = 100,000 毫分
// = 10⁻⁵ USD 精度）：
//   - InputImageTokenPricePerMillion / OutputImageTokenPricePerMillion：
//     毫分/1M image tokens（litellm per-token USD 换算 ×1e11 四舍五入取整；
//     gpt-image-2 官方形态 8e-06 → 800,000 / 3e-05 → 3,000,000）
//   - OutputCostPerImageMilli：毫分/张 flat（litellm output_cost_per_image USD
//     换算 ×1e5；aiml 0.054 → 5,400）——与 token 价不同换算系、不同单位，
//     计费（billing.ImageCost）不走 /1e6 除法
//
// 三价格列全 nullable：nil = 未配置该分量（行有效性 = 至少一个分量非 nil，
// 全 nil 行应用层拒绝落行/设价）。Source 复用 PricingSource（litellm|manual，
// 与 pricings 同款行级互斥优先级 manual > litellm）。
type ImagePrice struct {
	ID                              int64
	Model                           string
	InputImageTokenPricePerMillion  *int64          // 毫分/1M image tokens；nil = 无该分量价
	OutputImageTokenPricePerMillion *int64          // 毫分/1M image tokens；nil = 无该分量价
	OutputCostPerImageMilli         *int64          // 毫分/张 flat；nil = 不启用按张分量
	Raw                             json.RawMessage // litellm 原始条目完整镜像（manual 行恒 nil）
	Source                          PricingSource
	CreatedAt                       time.Time
	UpdatedAt                       time.Time
}

// HasAnyPrice 行有效性 = 至少一个价格分量非 nil（全 nil 行拒绝落行/设价；
// DB 无法约束，应用层判定）。
func (p *ImagePrice) HasAnyPrice() bool {
	return p.InputImageTokenPricePerMillion != nil ||
		p.OutputImageTokenPricePerMillion != nil ||
		p.OutputCostPerImageMilli != nil
}
