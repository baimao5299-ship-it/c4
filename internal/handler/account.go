package handler

import (
	"net/http"

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

// GetAccounts 账号列表（含运行时视图，ServerInterface）。Task 1：传空查询，行为不变。
func (h *AdminAPI) GetAccounts(w http.ResponseWriter, r *http.Request) {
	rows, _, err := h.svc.ListAccountViews(r.Context(), repository.ListQuery{})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]AccountView, 0, len(rows))
	for _, v := range rows {
		out = append(out, toAPIAccountView(v))
	}
	writeJSON(w, http.StatusOK, out)
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
