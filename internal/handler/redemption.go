package handler

import (
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/server"
	"go-proxy-mini/internal/service"
)

// 兑换码管理面（/admin/redemption-codes）：生成/列表/审计/失效。
// 生成参数校验在 service 层（validateGenerateRequest），handler 只做 JSON 解码
// 与 created_by 注入（决策 5：JWT 路径 context 有 UserID；静态 admin token → 0）。

// createdBy 取认证中间件注入的 platform_admin 用户 id（/admin JWT 路径）；
// 静态 admin token 路径未注入 → 0（系统创建，决策 5）。
func createdBy(r *http.Request) int64 {
	id, _ := server.UserIDFromContext(r.Context())
	return id
}

// pageToQuery 增强分页范式（page 1-based + page_size）→ repository.ListQuery。
// page 缺省/越下界按 1；page_size 缺省 20，越界（<1 或 >1000）→ ErrInvalidInput 400。
func pageToQuery(page, pageSize *int) (repository.ListQuery, error) {
	p := 1
	if page != nil && *page > 1 {
		p = *page
	}
	size := 20
	if pageSize != nil {
		size = *pageSize
	}
	if size < 1 || size > 1000 {
		return repository.ListQuery{}, service.ErrInvalidInput
	}
	return repository.ListQuery{Limit: size, Offset: (p - 1) * size}, nil
}

// PostRedemptionCodes 生成兑换码（1..count 个；count 默认 1，上限 1000，
// ServerInterface）。
func (h *AdminAPI) PostRedemptionCodes(w http.ResponseWriter, r *http.Request) {
	var in GenerateRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	codes, err := h.svc.GenerateCodes(r.Context(), service.GenerateRequest{
		Type:              domain.RedemptionType(in.Type),
		Value:             in.Value,
		Remark:            in.Remark,
		ExpiresAt:         in.ExpiresAt,
		ResourceExpiresAt: in.ResourceExpiresAt,
		MaxUses:           deref(in.MaxUses),
		Count:             deref(in.Count),
	}, createdBy(r))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]RedemptionCode, 0, len(codes))
	for _, c := range codes {
		out = append(out, toAPIRedemptionCode(c))
	}
	writeJSON(w, http.StatusOK, GenerateResponse{Codes: out})
}

// GetRedemptionCodes 兑换码列表（增强分页范式 + type/status 筛选 + sort 白名单，
// ServerInterface）。
func (h *AdminAPI) GetRedemptionCodes(w http.ResponseWriter, r *http.Request, params GetRedemptionCodesParams) {
	q, err := pageToQuery(params.Page, params.PageSize)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	q.Sort = deref(params.Sort)
	q.Order = string(deref(params.Order))
	var typ *domain.RedemptionType
	if params.Type != nil {
		t := domain.RedemptionType(*params.Type)
		typ = &t
	}
	var st *domain.RedemptionStatus
	if params.Status != nil {
		s := domain.RedemptionStatus(*params.Status)
		st = &s
	}
	rows, total, err := h.svc.ListCodes(r.Context(), q, typ, st)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]RedemptionCode, 0, len(rows))
	for _, c := range rows {
		out = append(out, toAPIRedemptionCode(c))
	}
	writeJSON(w, http.StatusOK, RedemptionCodeListResponse{Total: total, Rows: out})
}

// GetRedemptionCodesIdUses 某码的兑换记录（审计；码缺失 → 404，ServerInterface）。
func (h *AdminAPI) GetRedemptionCodesIdUses(w http.ResponseWriter, r *http.Request, id int64) {
	rows, total, err := h.svc.GetCodeUses(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]RedemptionUse, 0, len(rows))
	for _, u := range rows {
		out = append(out, toAPIRedemptionUse(u))
	}
	writeJSON(w, http.StatusOK, RedemptionUseListResponse{Total: total, Rows: out})
}

// PostRedemptionCodesIdDeactivate 单码失效（已失效 no-op 成功；缺失 → 404，
// ServerInterface）。
func (h *AdminAPI) PostRedemptionCodesIdDeactivate(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeactivateCode(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DeactivateResponse{Deactivated: true})
}

// PostRedemptionCodesBatchDeactivate 批量失效（单事务；已失效 no-op；缺失 id →
// 404 含缺失详情，ServerInterface）。
func (h *AdminAPI) PostRedemptionCodesBatchDeactivate(w http.ResponseWriter, r *http.Request) {
	var in BatchDeactivateRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := h.svc.DeactivateCodesBatch(r.Context(), ids)
	if err != nil {
		writeBatchServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BatchDeactivateResponse{Deactivated: int(n)})
}

// toAPIRedemptionCode 兑换码领域对象 → 契约类型。
func toAPIRedemptionCode(c *domain.RedemptionCode) RedemptionCode {
	return RedemptionCode{
		ID:                c.ID,
		Code:              c.Code,
		Type:              RedemptionType(c.Type),
		Value:             c.Value,
		Remark:            c.Remark,
		ExpiresAt:         c.ExpiresAt,
		ResourceExpiresAt: c.ResourceExpiresAt,
		MaxUses:           c.MaxUses,
		UsedCount:         c.UsedCount,
		Status:            RedemptionStatus(c.Status),
		CreatedBy:         c.CreatedBy,
		CreatedAt:         c.CreatedAt,
		UpdatedAt:         c.UpdatedAt,
	}
}

// toAPIRedemptionUse 兑换审计领域对象 → 契约类型。
func toAPIRedemptionUse(u *domain.RedemptionUse) RedemptionUse {
	return RedemptionUse{
		ID:                u.ID,
		CodeID:            u.CodeID,
		UserID:            u.UserID,
		Value:             u.Value,
		ResourceExpiresAt: u.ResourceExpiresAt,
		CreatedAt:         u.CreatedAt,
	}
}
