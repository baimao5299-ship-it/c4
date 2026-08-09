package handler

import (
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// 用户管理面（/admin/users）：余额字段在 API 边界换算 USD float64（内部存储
// 毫分——1 USD = 100,000 毫分；usdToMillis/millisToUSD）。价格倍率按组
// （T3.5 修正）经 /admin/groups/{id}/assignments 的 multipliers 设置，用户
// 本体无倍率字段。

// GetUsers 用户列表（platform_admin 专属；/admin 组中间件已鉴权，
// ServerInterface）。
func (h *AdminAPI) GetUsers(w http.ResponseWriter, r *http.Request, params GetUsersParams) {
	q := repository.ListQuery{
		Limit:  int(deref(params.Limit)),
		Offset: int(deref(params.Offset)),
		Email:  deref(params.Email),
		Sort:   deref(params.Sort),
		Order:  string(deref(params.Order)),
	}
	rows, total, err := h.svc.ListUsers(r.Context(), q)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]User, 0, len(rows))
	for _, u := range rows {
		out = append(out, toAPIUser(u))
	}
	writeJSON(w, http.StatusOK, UserListResponse{Total: total, Rows: out})
}

// PostUsers 创建用户（platform_admin 专属；email 唯一/密码长度校验在
// service；balance 输入 USD 换算毫分，ServerInterface）。
func (h *AdminAPI) PostUsers(w http.ResponseWriter, r *http.Request) {
	var in UserCreate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	role := domain.RoleUser
	if in.Role != nil {
		role = domain.Role(*in.Role)
	}
	status := domain.UserStatusActive
	if in.Status != nil {
		status = domain.UserStatus(*in.Status)
	}
	u, err := h.svc.CreateUser(r.Context(), in.Email, in.Password, role, status,
		deref(in.MaxConcurrency), usdToMillis(deref(in.Balance)))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIUser(u))
}

// PutUsersId 更新用户（role/status/max_concurrency/balance；变更即时生效——
// Auth 快照刷新，ServerInterface）。
func (h *AdminAPI) PutUsersId(w http.ResponseWriter, r *http.Request, id int64) {
	var in UserUpdate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	u, err := h.svc.GetUser(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	if in.Role != nil {
		u.Role = domain.Role(*in.Role)
	}
	if in.Status != nil {
		u.Status = domain.UserStatus(*in.Status)
	}
	if in.MaxConcurrency != nil {
		u.MaxConcurrency = *in.MaxConcurrency
	}
	if in.Balance != nil {
		u.Balance = usdToMillis(*in.Balance)
	}
	updated, err := h.svc.UpdateUser(r.Context(), u)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIUser(updated))
}
