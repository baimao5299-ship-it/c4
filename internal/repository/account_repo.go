package repository

import (
	"context"
	"fmt"
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
		return nil, errMissingID(err, id)
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) ListAccounts(ctx context.Context, q ListQuery) ([]*domain.Account, int64, error) {
	pred := r.client.Account.Query()
	if q.Name != "" {
		pred = pred.Where(account.NameContainsFold(q.Name))
	}
	if len(q.StatusList) > 0 {
		sts, err := toAccountStatusList(q.StatusList)
		if err != nil {
			return nil, 0, err
		}
		pred = pred.Where(account.StatusIn(sts...))
	}
	if q.TemplateID > 0 {
		pred = pred.Where(account.TemplateIDEQ(q.TemplateID))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(accountSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.WithTemplate().Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.Account, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainAccount(row))
	}
	return out, int64(total), nil
}

// toAccountStatusList 校验并转换多值 status 筛选。ent 生成的枚举没有 Valid() 方法
// （只有 StatusValidator），对照枚举常量校验；非法值返回 error（handler 已校验，repo 兜底）。
func toAccountStatusList(list []string) ([]account.Status, error) {
	out := make([]account.Status, 0, len(list))
	for _, s := range list {
		st := account.Status(s)
		switch st {
		case account.StatusActive, account.StatusUnhealthy, account.Status429, account.StatusDisabled:
		default:
			return nil, fmt.Errorf("invalid account status %q", s)
		}
		out = append(out, st)
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
	if err := r.client.Account.DeleteOneID(id).Exec(ctx); err != nil {
		return errMissingID(err, id)
	}
	return nil
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
