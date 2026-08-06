package handler

import (
	"net/http"

	"go-proxy-mini/internal/repository"
)

// PostGroups 创建分组（响应含明文 key，仅此一次，ServerInterface）。
func (h *AdminAPI) PostGroups(w http.ResponseWriter, r *http.Request) {
	var in GroupCreate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	g, raw, err := h.svc.CreateGroup(r.Context(), in.Name)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, CreateGroupResponse{Group: toAPIGroup(g), Key: raw})
}

// GetGroups 分组列表（分页/筛选/排序，ServerInterface）。
func (h *AdminAPI) GetGroups(w http.ResponseWriter, r *http.Request, params GetGroupsParams) {
	q := repository.ListQuery{
		Limit:  int(deref(params.Limit)),
		Offset: int(deref(params.Offset)),
		Name:   deref(params.Name),
		Sort:   deref(params.Sort),
		Order:  string(deref(params.Order)),
	}
	rows, total, err := h.svc.ListGroups(r.Context(), q)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]Group, 0, len(rows))
	for _, g := range rows {
		out = append(out, toAPIGroup(g))
	}
	writeJSON(w, http.StatusOK, GroupListResponse{Total: total, Rows: out})
}

// GetGroupsId 分组详情（ServerInterface）。
func (h *AdminAPI) GetGroupsId(w http.ResponseWriter, r *http.Request, id int64) {
	g, err := h.svc.GetGroup(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIGroup(g))
}

// PutGroupsId 全量更新分组（ServerInterface）。
func (h *AdminAPI) PutGroupsId(w http.ResponseWriter, r *http.Request, id int64) {
	var in GroupCreate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	g, err := h.svc.GetGroup(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	g.Name = in.Name
	updated, err := h.svc.UpdateGroup(r.Context(), g)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIGroup(updated))
}

// DeleteGroupsId 删除分组（ServerInterface）。
func (h *AdminAPI) DeleteGroupsId(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeleteGroup(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// PutGroupsIdAccounts 全量绑定账号集合（ServerInterface）。
func (h *AdminAPI) PutGroupsIdAccounts(w http.ResponseWriter, r *http.Request, id int64) {
	var in SetGroupAccountsBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := h.svc.SetGroupAccounts(r.Context(), id, in.AccountIds); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, UpdatedResponse{Updated: true})
}

// PostGroupsIdRotateKey 轮换分组 key（ServerInterface）。
func (h *AdminAPI) PostGroupsIdRotateKey(w http.ResponseWriter, r *http.Request, id int64) {
	raw, err := h.svc.RotateGroupKey(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, RotateKeyResponse{Key: raw})
}
