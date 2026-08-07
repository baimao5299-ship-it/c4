package handler

import (
	"errors"
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

// PostGroupsBatchDelete 批量删除分组（事务，全成或全败；key 清理由
// service 完成，ServerInterface）。
func (h *AdminAPI) PostGroupsBatchDelete(w http.ResponseWriter, r *http.Request) {
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
	if err := h.svc.DeleteGroupsBatch(r.Context(), ids); err != nil {
		writeBatchServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BatchDeleteResponse{Deleted: len(ids)})
}

// PostGroupsBatchUpdate 批量更新分组（fields 任意子集，ServerInterface）。
func (h *AdminAPI) PostGroupsBatchUpdate(w http.ResponseWriter, r *http.Request) {
	var in BatchUpdateGroupsBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := groupPatchFromBody(&in.Fields)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.UpdateGroupsBatch(r.Context(), ids, p); err != nil {
		writeBatchServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BatchUpdateResponse{Updated: len(ids)})
}

// groupPatchFromBody 生成类型 fields → repo patch（nil 字段 = 不更新；
// 当前仅 name 字段）。
func groupPatchFromBody(f *GroupPatch) (repository.GroupPatch, error) {
	if f.Name == nil {
		return repository.GroupPatch{}, errors.New("fields must contain at least one field")
	}
	return repository.GroupPatch{Name: f.Name}, nil
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
