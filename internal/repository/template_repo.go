package repository

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/template"
)

type TemplateRepo struct{ client *ent.Client }

func (r *TemplateRepo) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	row, err := r.client.Template.Create().
		SetName(t.Name).SetBaseURL(t.BaseURL).
		SetDefaultFormat(template.DefaultFormat(t.DefaultFormat)).
		SetModels(t.Models).
		SetModelFormats(toStringMap(t.ModelFormats)).
		SetModelMapping(t.ModelMapping).
		Save(ctx)
	if err != nil {
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
	if q.DefaultFormat != "" {
		pred = pred.Where(template.DefaultFormatEQ(template.DefaultFormat(q.DefaultFormat)))
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
		SetDefaultFormat(template.DefaultFormat(t.DefaultFormat)).
		SetModels(t.Models).
		SetModelFormats(toStringMap(t.ModelFormats)).
		SetModelMapping(t.ModelMapping).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainTemplate(row), nil
}

func (r *TemplateRepo) DeleteTemplate(ctx context.Context, id int64) error {
	return r.client.Template.DeleteOneID(id).Exec(ctx)
}

func toStringMap(m map[string]domain.RequestFormat) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = string(v)
	}
	return out
}
