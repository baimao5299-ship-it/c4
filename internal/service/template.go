package service

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/pkg/logx"
)

func (s *Service) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	if err := validateTemplate(t); err != nil {
		return nil, err
	}
	created, err := s.store.CreateTemplate(ctx, t)
	if err != nil {
		return nil, err
	}
	s.invalidate()
	if s.log != nil {
		s.log.Info("template created", logx.Int64("id", created.ID), logx.String("name", created.Name))
	}
	return created, nil
}

func (s *Service) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	return s.store.GetTemplate(ctx, id)
}

func (s *Service) ListTemplates(ctx context.Context, q repository.ListQuery) ([]*domain.Template, int64, error) {
	if err := validateListQuery(q, listSortFields["templates"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListTemplates(ctx, q)
}

func (s *Service) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	if err := validateTemplate(t); err != nil {
		return nil, err
	}
	updated, err := s.store.UpdateTemplate(ctx, t)
	if err != nil {
		return nil, err
	}
	s.invalidate()
	return updated, nil
}

func (s *Service) DeleteTemplate(ctx context.Context, id int64) error {
	err := s.store.DeleteTemplate(ctx, id)
	if err == nil {
		s.invalidate()
	}
	return err
}
