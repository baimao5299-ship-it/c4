// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/user"
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

// CreateTempBalance 创建临时额度行（注册赠品、兑换码兑换等）：每笔独立行、
// 独立到期（多笔不同到期共存，Phase 5 FEFO 扣费）。user_id 外键必存在
// （服务层先 CreateUser 拿到 id）。expiresAt/note 为 nil 时不落该列（nil = 永久）；
// 兑换码路径必非零（temp_balance 码 resource_expires_at 生成时必填，决策 4）。
// WithTx 事务内经 tx client 插入，随整体提交/回滚；普通 client 亦可用。
func (r *UserRepo) CreateTempBalance(ctx context.Context, userID int64, amount int64, expiresAt *time.Time, note *string) error {
	_, err := r.client.TempBalance.Create().
		SetUserID(userID).
		SetAmount(amount).
		SetNillableExpiresAt(expiresAt).
		SetNillableNote(note).
		Save(ctx)
	return err
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

// UserPatch 用户更新补丁（管理面 PUT patch 语义）：显式字段 = 请求显式提供的
// 字段；role/status 无条件写（无增量写者）；balance/max_concurrency 显式设置
// 时必带旧值条件（OldXxx = GET 快照，服务层重试时重读刷新）——旧值不满足
// （期间有扣费/并发变更）→ 0 行 → ErrConflict，绝不无条件覆盖并发增量
// （v02 核实：GET 快照陈旧值写回与 flusher 扣费双向覆盖，余额凭空复活）。
type UserPatch struct {
	ID               int64
	Role             *domain.Role
	Status           *domain.UserStatus
	MaxConcurrency   *int
	OldMaxConcurrency *int
	Balance          *int64
	OldBalance       *int64
}

// UpdateUser 按 patch 更新（email 不可变、密码走 UpdateUserPassword）。价格
// 倍率按组（T3.5 修正）挂在 group_assignments 上，用户本体无倍率字段——见
// GroupAssignmentRepo.SetMultiplier。
// 条件更新形态 `Update().Where(id, balance=old)`（评审 I-1 原子原语同族：不用
// FOR UPDATE 行锁——跨请求持锁与多实例不兼容）；0 行命中：用户缺失 →
// ErrNotFound，条件不满足（期间有扣费）→ ErrConflict（service 层重读重试
// ≤3 次，new 保持管理员显式意图）。成功路径 UPDATE + Get 返回行（与旧
// UpdateOneID 的 UPDATE + re-SELECT 同往返数）。
func (r *UserRepo) UpdateUser(ctx context.Context, p *UserPatch) (*domain.User, error) {
	if p.MaxConcurrency != nil && p.OldMaxConcurrency == nil {
		return nil, fmt.Errorf("repository: UpdateUser patch: max_concurrency set without old value")
	}
	if p.Balance != nil && p.OldBalance == nil {
		return nil, fmt.Errorf("repository: UpdateUser patch: balance set without old value")
	}
	upd := r.client.User.Update().Where(user.ID(p.ID))
	if p.Role != nil {
		upd.SetRole(user.Role(*p.Role))
	}
	if p.Status != nil {
		upd.SetStatus(user.Status(*p.Status))
	}
	if p.MaxConcurrency != nil {
		upd.Where(user.MaxConcurrency(*p.OldMaxConcurrency))
		upd.SetMaxConcurrency(*p.MaxConcurrency)
	}
	if p.Balance != nil {
		upd.Where(user.Balance(*p.OldBalance))
		upd.SetBalance(*p.Balance)
	}
	n, err := upd.Save(ctx)
	if err != nil {
		return nil, errMissingID(err, p.ID)
	}
	if n == 0 {
		// 0 行命中：用户缺失或条件不满足——回查区分（ErrNotFound / ErrConflict）。
		if _, err := r.client.User.Get(ctx, p.ID); err != nil {
			return nil, errMissingID(err, p.ID)
		}
		return nil, fmt.Errorf("%w: id=%d balance/max_concurrency changed concurrently", ErrConflict, p.ID)
	}
	row, err := r.client.User.Get(ctx, p.ID)
	if err != nil {
		return nil, errMissingID(err, p.ID)
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

// LoadBalances 全量余额快照（id → balance 毫分；Phase 5 计费余额预检数据源，
// billing.Balances.Reload 调用）。失败返回错误——调用方 fail-safe 保留旧快照。
// 用户专属倍率按组（T3.5 修正）挂在 group_assignments 上，不在此查询
// （见 GroupRepo.LoadAssignmentMultipliers）。
func (r *UserRepo) LoadBalances(ctx context.Context) (map[int64]int64, error) {
	rows, err := r.client.User.Query().Select(user.FieldID, user.FieldBalance).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int64, len(rows))
	for _, row := range rows {
		out[row.ID] = row.Balance
	}
	return out, nil
}
