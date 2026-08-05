package handler

import (
	"errors"
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/service"
)

func (h *Handler) createTemplate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string                          `json:"name"`
		BaseURL       string                          `json:"base_url"`
		DefaultFormat domain.RequestFormat            `json:"default_format"`
		Models        []string                        `json:"models"`
		ModelFormats  map[string]domain.RequestFormat `json:"model_formats"`
		ModelMapping  map[string]string               `json:"model_mapping"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	tpl := &domain.Template{
		Name: in.Name, BaseURL: in.BaseURL, DefaultFormat: in.DefaultFormat,
		Models: in.Models, ModelFormats: in.ModelFormats, ModelMapping: in.ModelMapping,
	}
	created, err := h.svc.CreateTemplate(r.Context(), tpl)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (h *Handler) listTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListTemplates(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) getTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	tpl, err := h.svc.GetTemplate(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tpl)
}

func (h *Handler) updateTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in domain.Template
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.ID = id
	updated, err := h.svc.UpdateTemplate(r.Context(), &in)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := h.svc.DeleteTemplate(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// writeServiceErr 统一把 service 错误映射为 HTTP 状态。
func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, service.ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}
