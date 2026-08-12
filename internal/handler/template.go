// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"errors"
	"net/http"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
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
		// 透传不做默认值兜底（评审 M-1：兜底在 service 层，防直接调用绕过）
		CredentialType:   credential.Type(deref(in.CredentialType)),
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

// GetTemplates 模板列表（分页/筛选/排序，ServerInterface）。
func (h *AdminAPI) GetTemplates(w http.ResponseWriter, r *http.Request, params GetTemplatesParams) {
	q := repository.ListQuery{
		Limit:  int(deref(params.Limit)),
		Offset: int(deref(params.Offset)),
		Name:   deref(params.Name),
		Sort:   deref(params.Sort),
		Order:  string(deref(params.Order)),
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
		Name:             in.Name,
		BaseURL:          in.BaseUrl,
		// 透传不做默认值兜底（评审 M-1：兜底在 service 层）
		CredentialType:   credential.Type(deref(in.CredentialType)),
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

// PostTemplatesBatchDelete 批量删除模板（事务，全成或全败，ServerInterface）。
func (h *AdminAPI) PostTemplatesBatchDelete(w http.ResponseWriter, r *http.Request) {
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
	if err := h.svc.DeleteTemplatesBatch(r.Context(), ids); err != nil {
		writeBatchServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BatchDeleteResponse{Deleted: len(ids)})
}

// PostTemplatesBatchUpdate 批量更新模板（fields 任意子集，ServerInterface）。
func (h *AdminAPI) PostTemplatesBatchUpdate(w http.ResponseWriter, r *http.Request) {
	var in BatchUpdateTemplatesBody
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := templatePatchFromBody(&in.Fields)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.UpdateTemplatesBatch(r.Context(), ids, p); err != nil {
		writeBatchServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BatchUpdateResponse{Updated: len(ids)})
}

// templatePatchFromBody 生成类型 fields → repo patch（nil 字段 = 不更新）。
// 空 fields（无任何字段）视为非法输入。
func templatePatchFromBody(f *TemplatePatch) (repository.TemplatePatch, error) {
	p := repository.TemplatePatch{
		Name:         f.Name,
		BaseURL:      f.BaseUrl,
		Models:       f.Models,
		ModelMapping: f.ModelMapping,
	}
	if f.FormatModels != nil {
		m := formatModelsFromBody(f.FormatModels)
		p.FormatModels = &m
	}
	if f.SupportedFormats != nil {
		fmts := make([]domain.RequestFormat, 0, len(*f.SupportedFormats))
		for _, sf := range *f.SupportedFormats {
			fmts = append(fmts, domain.RequestFormat(sf))
		}
		p.SupportedFormats = &fmts
	}
	if p.Name == nil && p.BaseURL == nil && p.SupportedFormats == nil &&
		p.Models == nil && p.FormatModels == nil && p.ModelMapping == nil {
		return repository.TemplatePatch{}, errors.New("fields must contain at least one field")
	}
	return p, nil
}

// writeServiceErr 统一把 service 错误映射为 HTTP 状态。404 输出 err.Error()
// （service 层已把缺失 id 详情包装进 ErrNotFound，与批量 404 同语义）。
func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrInvalidCredentials):
		writeErr(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, service.ErrSignupDisabled):
		writeErr(w, http.StatusForbidden, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}
