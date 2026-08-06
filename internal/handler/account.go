package handler

import (
	"errors"
	"net/http"
	"strings"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// PostAccounts 创建账号（ServerInterface）。
func (h *AdminAPI) PostAccounts(w http.ResponseWriter, r *http.Request) {
	var in AccountCreate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	created, err := h.svc.CreateAccount(r.Context(), &domain.Account{
		Name:           in.Name,
		TemplateID:     in.TemplateId,
		UpstreamKey:    in.UpstreamKey,
		Status:         domain.AccountStatus(deref(in.Status)),
		Weight:         deref(in.Weight),
		MaxConcurrency: deref(in.MaxConcurrency),
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIAccount(created))
}

// GetAccounts 账号列表（分页/筛选/排序，含运行时视图，ServerInterface）。
func (h *AdminAPI) GetAccounts(w http.ResponseWriter, r *http.Request, params GetAccountsParams) {
	q := repository.ListQuery{
		Limit:      int(deref(params.Limit)),
		Offset:     int(deref(params.Offset)),
		Name:       deref(params.Name),
		Sort:       deref(params.Sort),
		Order:      string(deref(params.Order)),
		TemplateID: deref(params.TemplateId),
	}
	if params.Status != nil && *params.Status != "" {
		sts := strings.Split(*params.Status, ",")
		for _, s := range sts {
			if !validAccountStatus(s) {
				writeErr(w, http.StatusBadRequest, "invalid status "+s)
				return
			}
		}
		q.StatusList = sts
	}
	rows, total, err := h.svc.ListAccountViews(r.Context(), q)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]AccountView, 0, len(rows))
	for _, v := range rows {
		out = append(out, toAPIAccountView(v))
	}
	writeJSON(w, http.StatusOK, AccountListResponse{Total: total, Rows: out})
}

// validAccountStatus 校验 status 多值参数（逗号分隔）的枚举值
// （active/unhealthy/429/disabled）。openapi 的 status 参数是纯 string
// （多值无法用 enum），生成类型不校验；必须在 handler 显式校验，
// 否则非法值落到 repo 兜底返回裸 error → 500（Task 1→2 handoff 硬性要求）。
func validAccountStatus(s string) bool {
	switch domain.AccountStatus(s) {
	case domain.StatusActive, domain.StatusUnhealthy, domain.Status429, domain.StatusDisabled:
		return true
	}
	return false
}

// GetAccountsId 账号详情（ServerInterface）。
func (h *AdminAPI) GetAccountsId(w http.ResponseWriter, r *http.Request, id int64) {
	acc, err := h.svc.GetAccount(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIAccount(acc))
}

// PutAccountsId 全量更新账号（ServerInterface）。
func (h *AdminAPI) PutAccountsId(w http.ResponseWriter, r *http.Request, id int64) {
	var in AccountCreate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	acc := &domain.Account{
		Name:           in.Name,
		TemplateID:     in.TemplateId,
		UpstreamKey:    in.UpstreamKey,
		Status:         domain.AccountStatus(deref(in.Status)),
		Weight:         deref(in.Weight),
		MaxConcurrency: deref(in.MaxConcurrency),
	}
	acc.ID = id
	updated, err := h.svc.UpdateAccount(r.Context(), acc)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIAccount(updated))
}

// DeleteAccountsId 删除账号（ServerInterface）。
func (h *AdminAPI) DeleteAccountsId(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeleteAccount(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// PostAccountsBatchDelete 批量删除账号（事务，全成或全败；删除后调度快照
// 失效由 service invalidate 完成，ServerInterface）。
func (h *AdminAPI) PostAccountsBatchDelete(w http.ResponseWriter, r *http.Request) {
	var in BatchDeleteBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.DeleteAccountsBatch(r.Context(), ids); err != nil {
		writeBatchServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BatchDeleteResponse{Deleted: len(ids)})
}

// PostAccountsBatchUpdate 批量更新账号（fields 任意子集；更新后调度快照
// 失效由 service invalidate 完成，ServerInterface）。
func (h *AdminAPI) PostAccountsBatchUpdate(w http.ResponseWriter, r *http.Request) {
	var in BatchUpdateAccountsBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := accountPatchFromBody(&in.Fields)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.UpdateAccountsBatch(r.Context(), ids, p); err != nil {
		writeBatchServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BatchUpdateResponse{Updated: len(ids)})
}

// accountPatchFromBody 生成类型 fields → repo patch（nil 字段 = 不更新）。
// 契约 status 枚举不参与解码校验（与列表参数一致），此处显式校验；
// 空 fields（无任何字段）视为非法输入。
func accountPatchFromBody(f *AccountPatch) (repository.AccountPatch, error) {
	if f.Status != nil && !validAccountStatus(string(*f.Status)) {
		return repository.AccountPatch{}, errors.New("invalid status " + string(*f.Status))
	}
	p := repository.AccountPatch{
		Name:           f.Name,
		TemplateID:     f.TemplateId,
		UpstreamKey:    f.UpstreamKey,
		Status:         (*domain.AccountStatus)(f.Status),
		Weight:         f.Weight,
		MaxConcurrency: f.MaxConcurrency,
	}
	if p.Name == nil && p.TemplateID == nil && p.UpstreamKey == nil &&
		p.Status == nil && p.Weight == nil && p.MaxConcurrency == nil {
		return repository.AccountPatch{}, errors.New("fields must contain at least one field")
	}
	return p, nil
}
