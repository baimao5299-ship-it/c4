package repository

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql/sqlgraph"

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
	// price_multiplier 0 = 未指定 → 不设置该列（DB 默认 10000 = ×1）。0 是合法
	// 倍率（免费组），显式设置经 UpdateGroup（或后续管理面契约）写入。
	q := r.client.Group.Create().
		SetName(g.Name).
		SetVisibility(group.Visibility(g.Visibility))
	if g.PriceMultiplier != 0 {
		q = q.SetPriceMultiplier(g.PriceMultiplier)
	}
	row, err := q.Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, g.Name)
		}
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
	// price_multiplier 恒写入（PUT 全量替换语义；管理面 PUT 读改写——fetch →
	// 改 name/visibility → 写回，未触及倍率时携带原值自然保留；显式 0 = 免费组）。
	row, err := r.client.Group.UpdateOneID(g.ID).
		SetName(g.Name).
		SetVisibility(group.Visibility(g.Visibility)).
		SetPriceMultiplier(g.PriceMultiplier).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, g.Name)
		}
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

// LoadGroupMultipliers 全量组倍率快照（id → 万分数；groups.price_multiplier
// NOT NULL 默认 10000——每行都有值；billing.Balances.Reload 调用）。独立方法
// 不并入 LoadGroupsAccounts（后者是账号路由快照，语义/带宽不同）。
func (r *GroupRepo) LoadGroupMultipliers(ctx context.Context) (map[int64]int, error) {
	rows, err := r.client.Group.Query().Select(group.FieldID, group.FieldPriceMultiplier).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int, len(rows))
	for _, row := range rows {
		out[row.ID] = row.PriceMultiplier
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
