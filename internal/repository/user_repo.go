package repository

import (
	"context"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/user"
)

// UserRepo 用户（顶层实体）持久化。
type UserRepo struct{ client *ent.Client }

func (r *UserRepo) CreateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	row, err := r.client.User.Create().
		SetEmail(u.Email).
		SetPasswordHash(u.PasswordHash).
		SetRole(user.Role(u.Role)).
		SetStatus(user.Status(u.Status)).
		SetMaxConcurrency(u.MaxConcurrency).
		SetBalance(u.Balance).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return toDomainUser(row), nil
}

// GetUser 按 id 取用户；缺失 → ErrNotFound。
func (r *UserRepo) GetUser(ctx context.Context, id int64) (*domain.User, error) {
	row, err := r.client.User.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainUser(row), nil
}

// GetUserByEmail 按邮箱取用户；未找到返回 (nil, nil)（登录/注册查重用）。
func (r *UserRepo) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	row, err := r.client.User.Query().Where(user.EmailEQ(email)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return toDomainUser(row), nil
}

func (r *UserRepo) ListUsers(ctx context.Context, q ListQuery) ([]*domain.User, int64, error) {
	pred := r.client.User.Query()
	if q.Email != "" {
		pred = pred.Where(user.EmailContainsFold(q.Email))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(userSortFields)
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
	out := make([]*domain.User, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainUser(row))
	}
	return out, int64(total), nil
}

// UpdateUser 更新 role/status/max_concurrency/balance（email 不可变、
// 密码走 UpdateUserPassword）。
func (r *UserRepo) UpdateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	row, err := r.client.User.UpdateOneID(u.ID).
		SetRole(user.Role(u.Role)).
		SetStatus(user.Status(u.Status)).
		SetMaxConcurrency(u.MaxConcurrency).
		SetBalance(u.Balance).
		Save(ctx)
	if err != nil {
		return nil, errMissingID(err, u.ID)
	}
	return toDomainUser(row), nil
}

func (r *UserRepo) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	_, err := r.client.User.UpdateOneID(id).SetPasswordHash(passwordHash).Save(ctx)
	return errMissingID(err, id)
}

// LoadUsers 全量用户状态快照（Auth 内存表：RequireJWT 用户状态校验，
// 用户变更走 invalidate → Reload 全量刷新，不用 DB 直查）。
func (r *UserRepo) LoadUsers(ctx context.Context) (map[int64]domain.UserStatus, error) {
	rows, err := r.client.User.Query().Select(user.FieldID, user.FieldStatus).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]domain.UserStatus, len(rows))
	for _, row := range rows {
		out[row.ID] = domain.UserStatus(row.Status)
	}
	return out, nil
}
