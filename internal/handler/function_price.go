// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 按单元计费功能类价格管理面（/admin/function-prices）：列表 / 单行 / 手动设价
// / 删除手动价（价格表三件套；search 起，audio/video 等未来 per-unit 端点复用）。
// 与 /admin/image-price 同形态，仅价格单位不同（USD/次 → 毫分/次，见换算函数
// 注释）；PUT/DELETE 的 {model} 由 chi 路径参数注入（生成契约签名）；错误映射
// 走 writeServiceErr（ErrNotFound → 404、ErrConflict → 409、ErrInvalidInput →
// 400——含"price_per_call 必填"校验）。

// GetFunctionPrices 按单元价列表（分页/筛选/排序，对齐 GetImagePrice），
// ServerInterface。
func (h *AdminAPI) GetFunctionPrices(w http.ResponseWriter, r *http.Request, params GetFunctionPricesParams) {
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
	rows, total, err := h.svc.ListFunctionPrice(r.Context(), q, src, deref(params.Model))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]FunctionPrice, 0, len(rows))
	for _, p := range rows {
		out = append(out, toAPIFunctionPrice(p))
	}
	writeJSON(w, http.StatusOK, FunctionPriceListResponse{Total: total, Rows: out})
}

// GetFunctionPricesModel 按 model 取单行（直读 DB 行，非快照——管理面展示
// 实际落库值；快照读 GetFunctionPrice 的 codex-search 默认兜底不在此面出现，
// 表行缺失 → 404），ServerInterface。
func (h *AdminAPI) GetFunctionPricesModel(w http.ResponseWriter, r *http.Request, model string) {
	p, err := h.svc.GetFunctionPriceRow(r.Context(), model)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIFunctionPrice(p))
}

// PutFunctionPricesModel 手动设按单元价（API 边界换算：收 USD/次——litellm
// 原生口径 input_cost_per_query → ×1e5 → 毫分/次（usdPerCallToMilli，**禁混用
// usdToMillis——虽同为 ×1e5 系数，但单位语义不同且防未来误改**）；price_per_call
// 必填（schema 未声明 required；service 校验兜底——缺省/null → 400，见 openapi
// FunctionPriceUpsert 描述）≥0（0 = 按次免费）。upsert 强制
// source=manual，可接管 litellm 行（codex-search 价可改），ServerInterface。
func (h *AdminAPI) PutFunctionPricesModel(w http.ResponseWriter, r *http.Request, model string) {
	var in FunctionPriceUpsert
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	p, err := h.svc.UpsertManualFunctionPrice(r.Context(), &repository.FunctionPriceManual{
		Model:        model,
		PricePerCall: usdPerCallToMilliPtr(in.PricePerCall),
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIFunctionPrice(p))
}

// DeleteFunctionPricesModel 删除手动按单元价：仅 source=manual 可删（litellm
// 行 → 409、不存在 → 404，service 错误映射；codex-search 种子行可删，重启
// bootstrap 幂等补回），ServerInterface。
func (h *AdminAPI) DeleteFunctionPricesModel(w http.ResponseWriter, r *http.Request, model string) {
	if err := h.svc.DeleteManualFunctionPrice(r.Context(), model); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// toAPIFunctionPrice 按单元价领域对象 → 契约类型（API 边界换算：毫分/次 →
// USD/次；raw 完整镜像不对外暴露，对齐 toAPIImagePrice）。
func toAPIFunctionPrice(p *domain.FunctionPrice) FunctionPrice {
	return FunctionPrice{
		Model:        p.Model,
		PricePerCall: milliPerCallToUSDPtr(p.PricePerCall),
		Source:       PricingSource(p.Source),
		CreatedAt:    p.CreatedAt,
		UpdatedAt:    p.UpdatedAt,
	}
}
