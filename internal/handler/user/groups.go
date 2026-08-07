package user

import (
	"net/http"
)

// GetUserGroups 可选组列表（public 全部 + 已授予 private；只读——组管理是
// 平台面职责，key 创建时在此选组，ServerInterface）。
func (h *UserAPI) GetUserGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListGroupsForUser(r.Context(), currentUserID(r))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]Group, 0, len(rows))
	for _, g := range rows {
		out = append(out, toAPIGroup(g))
	}
	writeJSON(w, http.StatusOK, out)
}
