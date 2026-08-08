package handler

import (
	"errors"
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/service"
)

// 模型价格管理面（/admin/pricing）：列表 / 手动设价 / 删除手动价 / 手动触发
// 同步。PUT/DELETE 的 {model} 由 chi 路径参数注入（生成契约签名）；错误映射
// 走 writeServiceErr（ErrNotFound → 404、ErrConflict → 409、ErrInvalidInput →
// 400），sync 拉取上游失败 → 502（ErrPriceFetch）。

// GetPricing 价格列表（增强分页范式 page/page_size + source/model 筛选 +
// sort 白名单，ServerInterface）。
func (h *AdminAPI) GetPricing(w http.ResponseWriter, r *http.Request, params GetPricingParams) {
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
	rows, total, err := h.svc.ListPricing(r.Context(), q, src, deref(params.Model))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]Pricing, 0, len(rows))
	for _, p := range rows {
		out = append(out, toAPIPricing(p))
	}
	writeJSON(w, http.StatusOK, PricingListResponse{Total: total, Rows: out})
}

// PutPricingModel 手动设价（毫分/1M tokens；upsert 强制 source=manual，可接管
// litellm 行；负数/model 空 → 400，service 校验）。可选字段（cache 价 + Phase 5
// 矩阵 22 列）缺省（nil）→ 清空（接管行该矩阵价清除，PUT 全量替换语义），
// ServerInterface。矩阵字段解码在 T4 契约扩展时随 openapi 生成补全。
func (h *AdminAPI) PutPricingModel(w http.ResponseWriter, r *http.Request, model string) {
	var in PricingUpsert
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	p, err := h.svc.UpsertManualPricing(r.Context(), &repository.PricingManual{
		Model:                        model,
		PromptPricePerMillion:        in.PromptPricePerMillion,
		CompletionPricePerMillion:    in.CompletionPricePerMillion,
		CacheReadPricePerMillion:     in.CacheReadPricePerMillion,
		CacheCreationPricePerMillion: in.CacheCreationPricePerMillion,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIPricing(p))
}

// DeletePricingModel 删除手动价：仅 source=manual 可删（litellm 行 → 409、
// 不存在 → 404，service 错误映射），ServerInterface。
func (h *AdminAPI) DeletePricingModel(w http.ResponseWriter, r *http.Request, model string) {
	if err := h.svc.DeleteManualPricing(r.Context(), model); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// PostPricingSync 手动触发一次价格同步（拉取官方价格表 → upsert → 快照重载；
// 成功 200 返回拉取统计；拉取上游失败 → 502、url 未配置 → 400），ServerInterface。
func (h *AdminAPI) PostPricingSync(w http.ResponseWriter, r *http.Request) {
	stats, err := h.svc.SyncPricingNow(r.Context())
	if err != nil {
		if errors.Is(err, service.ErrPriceFetch) {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, PricingSyncResponse{
		Rows: stats.Rows, Skipped: stats.Skipped, Updated: stats.Updated,
	})
}

// toAPIPricing 价格领域对象 → 契约类型（raw 完整镜像不对外暴露——前端无需
// litellm 原始条目；如需可后续加端点）。
func toAPIPricing(p *domain.Pricing) Pricing {
	return Pricing{
		Model:                        p.Model,
		PromptPricePerMillion:        p.PromptPricePerMillion,
		CompletionPricePerMillion:    p.CompletionPricePerMillion,
		MaxInputTokens:               p.MaxInputTokens,
		MaxOutputTokens:              p.MaxOutputTokens,
		CacheReadPricePerMillion:     p.CacheReadPricePerMillion,
		CacheCreationPricePerMillion: p.CacheCreationPricePerMillion,
		Provider:                     p.Provider,
		Mode:                         p.Mode,
		SupportsPromptCaching:        p.SupportsPromptCaching,
		Source:                       PricingSource(p.Source),
		CreatedAt:                    p.CreatedAt,
		UpdatedAt:                    p.UpdatedAt,
	}
}
