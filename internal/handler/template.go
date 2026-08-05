package handler

import (
	"errors"
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/service"
)

// templateBody 是模板请求体的可写字段（snake_case 与其余 admin 端点一致）。
// domain.Template 无 json tag，直接解码会丢弃 base_url/default_format 等键
// （评审发现：updateTemplate 原实现因此让文档化的 PUT 请求全部 400）。
type templateBody struct {
	Name          string                          `json:"name"`
	BaseURL       string                          `json:"base_url"`
	DefaultFormat domain.RequestFormat            `json:"default_format"`
	Models        []string                        `json:"models"`
	ModelFormats  map[string]domain.RequestFormat `json:"model_formats"`
	ModelMapping  map[string]string               `json:"model_mapping"`
}

func (b *templateBody) toTemplate() *domain.Template {
	return &domain.Template{
		Name: b.Name, BaseURL: b.BaseURL, DefaultFormat: b.DefaultFormat,
		Models: b.Models, ModelFormats: b.ModelFormats, ModelMapping: b.ModelMapping,
	}
}

func (h *Handler) createTemplate(w http.ResponseWriter, r *http.Request) {
	var in templateBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	created, err := h.svc.CreateTemplate(r.Context(), in.toTemplate())
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
	var in templateBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	tpl := in.toTemplate()
	tpl.ID = id
	updated, err := h.svc.UpdateTemplate(r.Context(), tpl)
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
