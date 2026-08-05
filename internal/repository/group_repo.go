package repository

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/group"
)

type GroupRepo struct{ client *ent.Client }

func (r *GroupRepo) Create(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	row, err := r.client.Group.Create().
		SetName(g.Name).SetKeyHash(g.KeyHash).SetKeyPrefix(g.KeyPrefix).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return &domain.Group{
		ID: row.ID, Name: row.Name, KeyHash: row.KeyHash, KeyPrefix: row.KeyPrefix,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *GroupRepo) Get(ctx context.Context, id int64) (*domain.Group, error) {
	row, err := r.client.Group.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &domain.Group{
		ID: row.ID, Name: row.Name, KeyHash: row.KeyHash, KeyPrefix: row.KeyPrefix,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *GroupRepo) List(ctx context.Context) ([]*domain.Group, error) {
	rows, err := r.client.Group.Query().Order(ent.Asc(group.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Group, 0, len(rows))
	for _, row := range rows {
		out = append(out, &domain.Group{
			ID: row.ID, Name: row.Name, KeyHash: row.KeyHash, KeyPrefix: row.KeyPrefix,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

func (r *GroupRepo) Update(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	row, err := r.client.Group.UpdateOneID(g.ID).
		SetName(g.Name).SetKeyHash(g.KeyHash).SetKeyPrefix(g.KeyPrefix).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return &domain.Group{
		ID: row.ID, Name: row.Name, KeyHash: row.KeyHash, KeyPrefix: row.KeyPrefix,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *GroupRepo) Delete(ctx context.Context, id int64) error {
	return r.client.Group.DeleteOneID(id).Exec(ctx)
}

// SetAccounts 全量替换分组账号成员（规格 §8）。
func (r *GroupRepo) SetAccounts(ctx context.Context, groupID int64, accountIDs []int64) error {
	_, err := r.client.Group.UpdateOneID(groupID).
		ClearAccounts().
		AddAccountIDs(accountIDs...).
		Save(ctx)
	return err
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

func (r *GroupRepo) LoadGroupKeys(ctx context.Context) (map[string]int64, error) {
	rows, err := r.client.Group.Query().Select(group.FieldKeyHash, group.FieldID).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.KeyHash] = row.ID
	}
	return out, nil
}
