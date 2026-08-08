package repository

import (
	"context"
	"fmt"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/user"
)

// UserRepo 用户（顶层实体）持久化。
type UserRepo struct {
	client *ent.Client
	// driver 为 raw SQL（原子资源方法）用：普通 client 与 tx client（WithTx 内）
	// 均可用——评审 I-1。ent v0.14 生成代码无 ExecContext/QueryContext，
	// raw SQL 经 dialect.Driver 统一执行。
	driver dialect.Driver
}

// UpdateUserBalance 原子增减余额（评审 I-1）：SET balance = balance + delta——
// 服务端原子，不读改写（并发增量不丢）；普通 client 与 tx client 均可用。
// 用户不存在 → ErrNotFound（0 行受影响 = 用户已删除，兑换编排整体回滚）。
func (r *UserRepo) UpdateUserBalance(ctx context.Context, userID, delta int64) error {
	u := sql.Update(user.Table).
		Set(user.FieldBalance, sql.ExprFunc(func(b *sql.Builder) {
			b.Ident(user.FieldBalance).WriteString(" + ").Arg(delta)
		})).
		Where(sql.EQ(user.FieldID, userID))
	n, err := execUpdate(ctx, r.driver, u)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, userID)
	}
	return nil
}

// UpdateUserMaxConcurrency 原子更新并发上限（评审 I-1）：0 = 不限语义特判入 SQL
// 单语句（CASE WHEN max_concurrency = 0 THEN value ELSE max_concurrency + value
// END）——当前不限直接设为 value，非 0 累加，无读改写竞态。
// 用户不存在 → ErrNotFound。
func (r *UserRepo) UpdateUserMaxConcurrency(ctx context.Context, userID int64, value int) error {
	u := sql.Update(user.Table).
		Set(user.FieldMaxConcurrency, sql.ExprFunc(func(b *sql.Builder) {
			b.WriteString("CASE WHEN ").
				Ident(user.FieldMaxConcurrency).WriteString(" = 0 THEN ")
			b.Arg(value)
			b.WriteString(" ELSE ").
				Ident(user.FieldMaxConcurrency).WriteString(" + ")
			b.Arg(value)
			b.WriteString(" END")
		})).
		Where(sql.EQ(user.FieldID, userID))
	n, err := execUpdate(ctx, r.driver, u)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, userID)
	}
	return nil
}

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
