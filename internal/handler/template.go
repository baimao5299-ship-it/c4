package handler

import (
	"errors"
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/service"
)

// formatsFromBody 契约格式数组 → 领域格式数组。
func formatsFromBody(in []TemplateCreateSupportedFormats) []domain.RequestFormat {
	out := make([]domain.RequestFormat, 0, len(in))
	for _, f := range in {
		out = append(out, domain.RequestFormat(f))
	}
	return out
}

// formatModelsFromBody 契约 map（格式 → 模型列表）→ 领域 map；nil 输入产出 nil。
func formatModelsFromBody(m *map[string][]string) map[domain.RequestFormat][]string {
	if m == nil {
		return nil
	}
	out := make(map[domain.RequestFormat][]string, len(*m))
	for k, v := range *m {
		out[domain.RequestFormat(k)] = v
	}
	return out
}

// PostTemplates 创建模板（ServerInterface）。
func (h *AdminAPI) PostTemplates(w http.ResponseWriter, r *http.Request) {
	var in TemplateCreate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	created, err := h.svc.CreateTemplate(r.Context(), &domain.Template{
		Name:             in.Name,
		BaseURL:          in.BaseUrl,
		SupportedFormats: formatsFromBody(in.SupportedFormats),
		Models:           deref(in.Models),
		FormatModels:     formatModelsFromBody(in.FormatModels),
		ModelMapping:     deref(in.ModelMapping),
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPITemplate(created))
}

// GetTemplates 模板列表（ServerInterface）。
func (h *AdminAPI) GetTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListTemplates(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]Template, 0, len(rows))
	for _, t := range rows {
		out = append(out, toAPITemplate(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// GetTemplatesId 模板详情（ServerInterface）。
func (h *AdminAPI) GetTemplatesId(w http.ResponseWriter, r *http.Request, id int64) {
	tpl, err := h.svc.GetTemplate(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPITemplate(tpl))
}

// PutTemplatesId 全量更新模板（ServerInterface）。
func (h *AdminAPI) PutTemplatesId(w http.ResponseWriter, r *http.Request, id int64) {
	var in TemplateCreate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	tpl := &domain.Template{
		Name:             in.Name,
		BaseURL:          in.BaseUrl,
		SupportedFormats: formatsFromBody(in.SupportedFormats),
		Models:           deref(in.Models),
		FormatModels:     formatModelsFromBody(in.FormatModels),
		ModelMapping:     deref(in.ModelMapping),
	}
	tpl.ID = id
	updated, err := h.svc.UpdateTemplate(r.Context(), tpl)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPITemplate(updated))
}

// DeleteTemplatesId 删除模板（ServerInterface）。
func (h *AdminAPI) DeleteTemplatesId(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeleteTemplate(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
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
