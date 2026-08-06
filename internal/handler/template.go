package handler

import (
	"errors"
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/service"
)

// toDomainFormats 契约 map（值类型为生成的 RequestFormat）→ 领域 map。
func toDomainFormats(m *map[string]RequestFormat) map[string]domain.RequestFormat {
	if m == nil {
		return nil
	}
	out := make(map[string]domain.RequestFormat, len(*m))
	for k, v := range *m {
		out[k] = domain.RequestFormat(v)
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
		Name:          in.Name,
		BaseURL:       in.BaseUrl,
		DefaultFormat: domain.RequestFormat(in.DefaultFormat),
		Models:        deref(in.Models),
		ModelFormats:  toDomainFormats(in.ModelFormats),
		ModelMapping:  deref(in.ModelMapping),
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPITemplate(created))
}

// GetTemplates 模板列表（分页/筛选/排序，ServerInterface）。
func (h *AdminAPI) GetTemplates(w http.ResponseWriter, r *http.Request, params GetTemplatesParams) {
	q := repository.ListQuery{
		Limit:         int(deref(params.Limit)),
		Offset:        int(deref(params.Offset)),
		Name:          deref(params.Name),
		Sort:          deref(params.Sort),
		Order:         string(deref(params.Order)),
		DefaultFormat: string(deref(params.DefaultFormat)),
	}
	rows, total, err := h.svc.ListTemplates(r.Context(), q)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]Template, 0, len(rows))
	for _, t := range rows {
		out = append(out, toAPITemplate(t))
	}
	writeJSON(w, http.StatusOK, TemplateListResponse{Total: total, Rows: out})
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
		Name:          in.Name,
		BaseURL:       in.BaseUrl,
		DefaultFormat: domain.RequestFormat(in.DefaultFormat),
		Models:        deref(in.Models),
		ModelFormats:  toDomainFormats(in.ModelFormats),
		ModelMapping:  deref(in.ModelMapping),
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
