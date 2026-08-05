package repository

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/template"
)

type TemplateRepo struct{ client *ent.Client }

func (r *TemplateRepo) Create(ctx context.Context, t *domain.Template) (*domain.Template, error) {
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

func (r *TemplateRepo) Get(ctx context.Context, id int64) (*domain.Template, error) {
	row, err := r.client.Template.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return toDomainTemplate(row), nil
}

func (r *TemplateRepo) List(ctx context.Context) ([]*domain.Template, error) {
	rows, err := r.client.Template.Query().Order(ent.Asc(template.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Template, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainTemplate(row))
	}
	return out, nil
}

func (r *TemplateRepo) Update(ctx context.Context, t *domain.Template) (*domain.Template, error) {
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

func (r *TemplateRepo) Delete(ctx context.Context, id int64) error {
	return r.client.Template.DeleteOneID(id).Exec(ctx)
}

func toStringMap(m map[string]domain.RequestFormat) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = string(v)
	}
	return out
}
