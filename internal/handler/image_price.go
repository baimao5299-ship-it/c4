// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 图片生成价格管理面（/admin/image-price）：列表 / 手动设价 / 删除手动价
// （Task A 数据面；images 端点计费价格来源）。PUT/DELETE 的 {model} 由 chi
// 路径参数注入（生成契约签名）；错误映射走 writeServiceErr（ErrNotFound →
// 404、ErrConflict → 409、ErrInvalidInput → 400——含"至少一价非 nil"校验）。
// 与 /admin/pricing 同形态，仅价格单位不同（见换算函数注释）。

// GetImagePrice 图片价格列表（分页/筛选/排序，对齐 GetPricing），ServerInterface。
func (h *AdminAPI) GetImagePrice(w http.ResponseWriter, r *http.Request, params GetImagePriceParams) {
	q, err := pageToQuery(params.Page, params.PageSize)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	q.Sort = deref(params.Sort)
	q.Order = string(deref(params.Order))
	var src *domain.PricingSource
	if params.Source != nil {
		s := domain.PricingSource(*params.Source)
		src = &s
	}
	rows, total, err := h.svc.ListImagePrice(r.Context(), q, src, deref(params.Model))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]ImagePrice, 0, len(rows))
	for _, p := range rows {
		out = append(out, toAPIImagePrice(p))
	}
	writeJSON(w, http.StatusOK, ImagePriceListResponse{Total: total, Rows: out})
}

// PutImagePriceModel 手动设图片价格（API 边界换算——**单位规则与 pricings 不同**）：
// token 价收 USD per image token（litellm 原生口径；openapi 字段名
// *_price_per_million 为历史命名，实际为 per-token 价）→ ×1e11 → 毫分/1M
// （usdPerMillionToMillis，独立换算函数）；per-image 收 USD/张 → ×1e5 →
// 毫分/张（usdPerImageToMilli，**禁混用 usdToMillis——虽同为 ×1e5 系数，但
// 单位语义不同且防未来误改**）。三分量全缺省（nil）→ 400（service 校验：行
// 有效性 = 至少一价非 nil）。upsert 强制 source=manual，可接管 litellm 行，
// ServerInterface。
func (h *AdminAPI) PutImagePriceModel(w http.ResponseWriter, r *http.Request, model string) {
	var in ImagePriceUpsert
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	p, err := h.svc.UpsertManualImagePrice(r.Context(), &repository.ImagePriceManual{
		Model:                           model,
		InputImageTokenPricePerMillion:  usdPerMillionToMillisPtr(in.InputImageTokenPricePerMillion),
		OutputImageTokenPricePerMillion: usdPerMillionToMillisPtr(in.OutputImageTokenPricePerMillion),
		OutputCostPerImageMilli:         usdPerImageToMilliPtr(in.OutputCostPerImage),
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIImagePrice(p))
}

// DeleteImagePriceModel 删除手动图片价格：仅 source=manual 可删（litellm 行 →
// 409、不存在 → 404，service 错误映射），ServerInterface。
func (h *AdminAPI) DeleteImagePriceModel(w http.ResponseWriter, r *http.Request, model string) {
	if err := h.svc.DeleteManualImagePrice(r.Context(), model); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// toAPIImagePrice 图片价格领域对象 → 契约类型（API 边界换算：毫分/1M →
// USD per image token、毫分/张 → USD/张；raw 完整镜像不对外暴露，对齐
// toAPIPricing）。
func toAPIImagePrice(p *domain.ImagePrice) ImagePrice {
	return ImagePrice{
		Model:                           p.Model,
		InputImageTokenPricePerMillion:  millisPerMillionToUSDPtr(p.InputImageTokenPricePerMillion),
		OutputImageTokenPricePerMillion: millisPerMillionToUSDPtr(p.OutputImageTokenPricePerMillion),
		OutputCostPerImage:              milliPerImageToUSDPtr(p.OutputCostPerImageMilli),
		Source:                          PricingSource(p.Source),
		CreatedAt:                       p.CreatedAt,
		UpdatedAt:                       p.UpdatedAt,
	}
}
