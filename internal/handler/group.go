package handler

import (
	"net/http"
)

func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	g, raw, err := h.svc.CreateGroup(r.Context(), in.Name)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group": g,
		"key":   raw, // 仅此一次明文
	})
}

func (h *Handler) listGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListGroups(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) getGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	g, err := h.svc.GetGroup(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (h *Handler) updateGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
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
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := h.svc.DeleteGroup(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (h *Handler) setGroupAccounts(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := h.svc.SetGroupAccounts(r.Context(), id, in.AccountIDs); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (h *Handler) rotateGroupKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	raw, err := h.svc.RotateGroupKey(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": raw})
}
