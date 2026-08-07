package handler

import (
	"net/http"
)

// GetAdminSettings 全部设置（默认值 + DB 覆盖；ServerInterface）。
func (h *AdminAPI) GetAdminSettings(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.GetSettings(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]Setting, 0, len(rows))
	for _, s := range rows {
		out = append(out, toAPISetting(s))
	}
	writeJSON(w, http.StatusOK, out)
}

// PutAdminSettings 更新设置（类型化校验在 service；返回更新后全部设置，
// ServerInterface）。
func (h *AdminAPI) PutAdminSettings(w http.ResponseWriter, r *http.Request) {
	var in SettingUpdate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if _, err := h.svc.UpdateSetting(r.Context(), in.Key, in.Value); err != nil {
		writeServiceErr(w, err)
		return
	}
	h.GetAdminSettings(w, r)
}
