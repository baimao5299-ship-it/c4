package handler

import (
	"net/http"

	"go-proxy-mini/internal/domain"
)

// accountBody 是账号请求体的可写字段（snake_case 与其余 admin 端点一致）。
// domain.Account 无 json tag，直接解码会丢弃 template_id/upstream_key 等键
// （计划/brief 测试即按 snake_case 发送，此处在 handler 层对齐 wire 格式，
// 不动 domain——Task 6 并行使用该类型）。
type accountBody struct {
	Name           string               `json:"name"`
	TemplateID     int64                `json:"template_id"`
	UpstreamKey    string               `json:"upstream_key"`
	Status         domain.AccountStatus `json:"status"`
	Weight         int                  `json:"weight"`
	MaxConcurrency int                  `json:"max_concurrency"`
}

func (b *accountBody) toAccount() *domain.Account {
	return &domain.Account{
		Name: b.Name, TemplateID: b.TemplateID, UpstreamKey: b.UpstreamKey,
		Status: b.Status, Weight: b.Weight, MaxConcurrency: b.MaxConcurrency,
	}
}

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	var in accountBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	created, err := h.svc.CreateAccount(r.Context(), in.toAccount())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListAccountViews(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) getAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	acc, err := h.svc.GetAccount(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

func (h *Handler) updateAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in accountBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	acc := in.toAccount()
	acc.ID = id
	updated, err := h.svc.UpdateAccount(r.Context(), acc)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := h.svc.DeleteAccount(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
