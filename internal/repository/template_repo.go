package repository

import (
	"context"
	"fmt"

	"entgo.io/ent/dialect/sql/sqlgraph"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/template"
)

type TemplateRepo struct{ client *ent.Client }

func (r *TemplateRepo) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	row, err := r.client.Template.Create().
		SetName(t.Name).SetBaseURL(t.BaseURL).
		// 全字段 Set（含 credential_type）：空串兜底在 service 层（M-1，防默认值被绕过）
		SetCredentialType(string(t.CredentialType)).
		SetSupportedFormats(formatsToStrings(t.SupportedFormats)).
		SetModels(t.Models).
		SetFormatModels(formatModelsToStrings(t.FormatModels)).
		SetModelMapping(t.ModelMapping).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, t.Name)
		}
		return nil, err
	}
	return toDomainTemplate(row), nil
}

func (r *TemplateRepo) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	row, err := r.client.Template.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainTemplate(row), nil
}

func (r *TemplateRepo) ListTemplates(ctx context.Context, q ListQuery) ([]*domain.Template, int64, error) {
	pred := r.client.Template.Query()
	if q.Name != "" {
		pred = pred.Where(template.NameContainsFold(q.Name))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(templateSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.Template, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainTemplate(row))
	}
	return out, int64(total), nil
}

func (r *TemplateRepo) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	row, err := r.client.Template.UpdateOneID(t.ID).
		SetName(t.Name).SetBaseURL(t.BaseURL).
		// 全字段 Set（含 credential_type）：空串兜底在 service 层（M-1）
		SetCredentialType(string(t.CredentialType)).
		SetSupportedFormats(formatsToStrings(t.SupportedFormats)).
		SetModels(t.Models).
		SetFormatModels(formatModelsToStrings(t.FormatModels)).
		SetModelMapping(t.ModelMapping).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, t.Name)
		}
		return nil, err
	}
	return toDomainTemplate(row), nil
}

func (r *TemplateRepo) DeleteTemplate(ctx context.Context, id int64) error {
	if err := r.client.Template.DeleteOneID(id).Exec(ctx); err != nil {
		return errMissingID(err, id)
	}
	return nil
}

// formatsToStrings 领域格式数组 → ent 字符串数组。
func formatsToStrings(fs []domain.RequestFormat) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f))
	}
	return out
}

// formatModelsToStrings 领域 map（键为 RequestFormat）→ ent map（键为 string）。
func formatModelsToStrings(m map[domain.RequestFormat][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}
