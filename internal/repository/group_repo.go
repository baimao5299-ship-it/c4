package repository

import (
	"context"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/group"
)

// GroupRepo 同时承担调度器 Loader 的账号状态回写（UpdateAccountStatus 委托 AccountRepo，
// 由 repository.New 注入；调度器按单个 loader 对象获取数据源）。
type GroupRepo struct {
	client   *ent.Client
	accounts *AccountRepo
}

func (r *GroupRepo) CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	row, err := r.client.Group.Create().
		SetName(g.Name).
		SetVisibility(group.Visibility(g.Visibility)).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainGroup(row), nil
}

func (r *GroupRepo) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	row, err := r.client.Group.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainGroup(row), nil
}

func (r *GroupRepo) ListGroups(ctx context.Context, q ListQuery) ([]*domain.Group, int64, error) {
	pred := r.client.Group.Query()
	if q.Name != "" {
		pred = pred.Where(group.NameContainsFold(q.Name))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(groupSortFields)
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
	out := make([]*domain.Group, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainGroup(row))
	}
	return out, int64(total), nil
}

func (r *GroupRepo) UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	row, err := r.client.Group.UpdateOneID(g.ID).
		SetName(g.Name).
		SetVisibility(group.Visibility(g.Visibility)).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainGroup(row), nil
}

func (r *GroupRepo) DeleteGroup(ctx context.Context, id int64) error {
	if err := r.client.Group.DeleteOneID(id).Exec(ctx); err != nil {
		return errMissingID(err, id)
	}
	return nil
}

func (r *GroupRepo) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	groups, err := r.client.Group.Query().
		WithAccounts(func(q *ent.AccountQuery) { q.WithTemplate() }).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]*domain.Account, len(groups))
	for _, g := range groups {
		var accs []*domain.Account
		for _, a := range g.Edges.Accounts {
			accs = append(accs, toDomainAccount(a))
		}
		out[g.ID] = accs
	}
	return out, nil
}

func (r *GroupRepo) LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error) {
	accs, err := r.client.Group.Query().
		Where(group.IDEQ(groupID)).
		QueryAccounts().
		WithTemplate().
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Account, 0, len(accs))
	for _, a := range accs {
		out = append(out, toDomainAccount(a))
	}
	return out, nil
}

// UpdateAccountStatus 满足 scheduler.Loader：账号状态回写委托 AccountRepo。
func (r *GroupRepo) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldownUntil *time.Time, lastError *string, weight *int) error {
	return r.accounts.UpdateAccountStatus(ctx, id, status, cooldownUntil, lastError, weight)
}
