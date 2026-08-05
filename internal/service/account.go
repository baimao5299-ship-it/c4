package service

import (
	"context"

	"go-proxy-mini/internal/domain"
)

func (s *Service) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	if err := validateAccount(a); err != nil {
		return nil, err
	}
	if _, err := s.store.GetTemplate(ctx, a.TemplateID); err != nil {
		return nil, err
	}
	created, err := s.store.CreateAccount(ctx, a)
	if err != nil {
		return nil, err
	}
	s.invalidate()
	return created, nil
}

func (s *Service) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	return s.store.GetAccount(ctx, id)
}

func (s *Service) ListAccounts(ctx context.Context) ([]*domain.Account, error) {
	return s.store.ListAccounts(ctx)
}

func (s *Service) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	if err := validateAccount(a); err != nil {
		return nil, err
	}
	updated, err := s.store.UpdateAccount(ctx, a)
	if err != nil {
		return nil, err
	}
	s.invalidate()
	return updated, nil
}

func (s *Service) DeleteAccount(ctx context.Context, id int64) error {
	err := s.store.DeleteAccount(ctx, id)
	if err == nil {
		s.invalidate()
	}
	return err
}

// AccountView 是账号的管理端视图（含调度器运行时信息）。
type AccountView struct {
	*domain.Account
	Concurrency int64   `json:"concurrency"`
	ErrRate     float64 `json:"err_rate"`
	ErrCount    int     `json:"err_count"`
}

func (s *Service) ListAccountViews(ctx context.Context) ([]*AccountView, error) {
	accs, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*AccountView, 0, len(accs))
	for _, a := range accs {
		v := &AccountView{Account: a}
		if s.sched != nil {
			if ri, ok := s.sched.Runtime(a.ID); ok {
				v.Concurrency, v.ErrRate, v.ErrCount = ri.Concurrency, ri.ErrRate, ri.ErrCount
			}
		}
		out = append(out, v)
	}
	return out, nil
}
