package repository

import (
	"context"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/account"
)

type AccountRepo struct{ client *ent.Client }

func (r *AccountRepo) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	row, err := r.client.Account.Create().
		SetName(a.Name).SetTemplateID(a.TemplateID).SetUpstreamKey(a.UpstreamKey).
		SetWeight(a.Weight).SetMaxConcurrency(a.MaxConcurrency).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	row, err := r.client.Account.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) ListAccounts(ctx context.Context) ([]*domain.Account, error) {
	rows, err := r.client.Account.Query().WithTemplate().Order(ent.Asc(account.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Account, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainAccount(row))
	}
	return out, nil
}

func (r *AccountRepo) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	row, err := r.client.Account.UpdateOneID(a.ID).
		SetName(a.Name).SetTemplateID(a.TemplateID).SetUpstreamKey(a.UpstreamKey).
		SetWeight(a.Weight).SetMaxConcurrency(a.MaxConcurrency).
		SetStatus(account.Status(a.Status)).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) DeleteAccount(ctx context.Context, id int64) error {
	return r.client.Account.DeleteOneID(id).Exec(ctx)
}

func (r *AccountRepo) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldownUntil *time.Time, lastError *string) error {
	u := r.client.Account.UpdateOneID(id).SetStatus(account.Status(status))
	if cooldownUntil != nil {
		u = u.SetCooldownUntil(*cooldownUntil)
	} else {
		u = u.ClearCooldownUntil()
	}
	if lastError != nil {
		u = u.SetLastError(*lastError)
	} else {
		u = u.ClearLastError()
	}
	_, err := u.Save(ctx)
	return err
}
