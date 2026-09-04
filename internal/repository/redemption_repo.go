// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"unicode"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/redemptioncode"
	"github.com/is7qin/c3api/internal/ent/redemptionuse"
	"github.com/is7qin/c3api/internal/ent/user"
)

// RedemptionRepo 兑换码 + 兑换审计持久化。
type RedemptionRepo struct {
	client *ent.Client
	// driver 为 raw SQL（IncrementUsed 条件递增）用：普通 client 与 tx client
	// （WithTx 内）均可用——评审 I-1。
	driver dialect.Driver
}

// DeactivateLegacyCodes retires every active code that is not the current
// twelve-letter format. Rows and uses remain intact for attribution/audit.
func (r *RedemptionRepo) DeactivateLegacyCodes(ctx context.Context) (int64, error) {
	rows, err := r.client.RedemptionCode.Query().
		Where(redemptioncode.StatusEQ(redemptioncode.StatusActive)).All(ctx)
	if err != nil {
		return 0, err
	}
	ids := make([]int64, 0)
	for _, row := range rows {
		if !isCurrentCode(row.Code) {
			ids = append(ids, row.ID)
		}
	}
	return r.DeactivateCodes(ctx, ids)
}

// DeactivateAllActiveCodes is the explicit one-time inventory reset. It keeps
// every code and use row for traceability and only prevents future redemption.
func (r *RedemptionRepo) DeactivateAllActiveCodes(ctx context.Context) (int64, error) {
	n, err := r.client.RedemptionCode.Update().
		Where(redemptioncode.StatusEQ(redemptioncode.StatusActive)).
		SetStatus(redemptioncode.StatusDisabled).Save(ctx)
	return int64(n), err
}

func isCurrentCode(code string) bool {
	if len(code) != 12 {
		return false
	}
	for _, ch := range code {
		if ch > unicode.MaxASCII || ch < 'A' || ch > 'Z' {
			return false
		}
	}
	return true
}

// CreateCodes 批量插入兑换码（单条 INSERT 多 VALUES；全字段 Set，含 status/used_count，
// 无默认值依赖）；code 唯一冲突 → ErrConflict（service 层重试换码，N=5）。
// Save 返回落库行：把 DB 分配的 id/时间戳回填到入参（响应 {codes: [...]} 需
// 完整可用——Task 3 评审发现 Exec 丢弃结果行导致响应 id=0）。
func (r *RedemptionRepo) CreateCodes(ctx context.Context, codes []*domain.RedemptionCode) error {
	if len(codes) == 0 {
		return nil
	}
	builders := make([]*ent.RedemptionCodeCreate, 0, len(codes))
	for _, c := range codes {
		builders = append(builders, r.client.RedemptionCode.Create().
			SetCode(c.Code).
			SetType(redemptioncode.Type(c.Type)).
			SetValue(c.Value).
			SetNillableGroupID(c.GroupID).
			SetNillableRemark(c.Remark).
			SetNillableExpiresAt(c.ExpiresAt).
			SetNillableResourceExpiresAt(c.ResourceExpiresAt).
			SetMaxUses(c.MaxUses).
			SetUsedCount(c.UsedCount).
			SetStatus(redemptioncode.Status(c.Status)).
			SetCreatedBy(c.CreatedBy))
	}
	created, err := r.client.RedemptionCode.CreateBulk(builders...).Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return fmt.Errorf("%w: code unique conflict (batch insert all failed)", ErrConflict)
		}
		return err
	}
	for i, row := range created {
		codes[i].ID = row.ID
		codes[i].CreatedAt = row.CreatedAt
		codes[i].UpdatedAt = row.UpdatedAt
	}
	return nil
}

// GetByCode 按 code 取兑换码；缺失 → ErrNotFound（含 code 详情）。
func (r *RedemptionRepo) GetByCode(ctx context.Context, code string) (*domain.RedemptionCode, error) {
	row, err := r.client.RedemptionCode.Query().
		Where(redemptioncode.CodeEQ(code)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: code=%q", ErrNotFound, code)
		}
		return nil, err
	}
	return toDomainRedemptionCode(row), nil
}

// GetCode 按 id 取兑换码；缺失 → ErrNotFound。
func (r *RedemptionRepo) GetCode(ctx context.Context, id int64) (*domain.RedemptionCode, error) {
	row, err := r.client.RedemptionCode.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainRedemptionCode(row), nil
}

// ListCodes 分页/筛选（type、status；nil = 不过滤）/排序（sort 白名单，
// 非法值 → ErrInvalidSort）。q.Name 无对应筛选字段（code 精确匹配走 GetByCode）。
func (r *RedemptionRepo) ListCodes(ctx context.Context, q ListQuery, typ *domain.RedemptionType, status *domain.RedemptionStatus) ([]*domain.RedemptionCode, int64, error) {
	pred := r.client.RedemptionCode.Query()
	if typ != nil {
		pred = pred.Where(redemptioncode.TypeEQ(redemptioncode.Type(*typ)))
	}
	if status != nil {
		pred = pred.Where(redemptioncode.StatusEQ(redemptioncode.Status(*status)))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(redemptionCodeSortFields)
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
	out := make([]*domain.RedemptionCode, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainRedemptionCode(row))
	}
	return out, int64(total), nil
}

// ListCodeUses 某码的兑换记录（审计），分页/排序（sort 白名单）。
func (r *RedemptionRepo) ListCodeUses(ctx context.Context, codeID int64, q ListQuery) ([]*domain.RedemptionUse, int64, error) {
	pred := r.client.RedemptionUse.Query().Where(redemptionuse.CodeID(codeID))
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(redemptionUseSortFields)
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
	out := make([]*domain.RedemptionUse, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainRedemptionUse(row))
	}
	return out, int64(total), nil
}

// ListUsesByUser 某用户的兑换记录（/api/user/redemptions）：use + 码联查
// （WithCode 边，Required 恒非空；码的 type/remark 随记录返回），分页/排序
// （sort 白名单）。
func (r *RedemptionRepo) ListUsesByUser(ctx context.Context, userID int64, q ListQuery) ([]*domain.RedemptionRecord, int64, error) {
	pred := r.client.RedemptionUse.Query().Where(redemptionuse.UserID(userID))
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(redemptionUseSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.WithCode().Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.RedemptionRecord, 0, len(rows))
	for _, row := range rows {
		rec := toDomainRedemptionRecord(row)
		if code := row.Edges.Code; code != nil { // Required 边恒非空，防御性判空
			rec.Code = code.Code
			rec.CodeType = domain.RedemptionType(code.Type)
			rec.Remark = code.Remark
		}
		out = append(out, rec)
	}
	return out, int64(total), nil
}

// ListHistory 管理面全量兑换历史：按兑换码/用户/类型筛选，使用一次带 code
// 边的查询返回完整审计行，避免前端逐码请求造成 N+1。排序字段与单码审计
// 共用 redemptionUseSortFields，默认按 id 倒序。
func (r *RedemptionRepo) ListHistory(ctx context.Context, q ListQuery, codeID, userID int64, typ *domain.RedemptionType) ([]*domain.RedemptionHistory, int64, error) {
	pred := r.client.RedemptionUse.Query()
	if codeID > 0 {
		pred = pred.Where(redemptionuse.CodeIDEQ(codeID))
	}
	if userID > 0 {
		pred = pred.Where(redemptionuse.UserIDEQ(userID))
	}
	if typ != nil {
		pred = pred.Where(redemptionuse.HasCodeWith(redemptioncode.TypeEQ(redemptioncode.Type(*typ))))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(redemptionUseSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.WithCode().Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.RedemptionHistory, 0, len(rows))
	userIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		userIDs = append(userIDs, row.UserID)
	}
	emails := make(map[int64]string, len(userIDs))
	if len(userIDs) > 0 {
		users, usersErr := r.client.User.Query().Where(user.IDIn(userIDs...)).All(ctx)
		if usersErr != nil {
			return nil, 0, usersErr
		}
		for _, u := range users {
			emails[u.ID] = u.Email
		}
	}
	for _, row := range rows {
		h := &domain.RedemptionHistory{
			ID: row.ID, CodeID: row.CodeID, UserID: row.UserID,
			UserEmail: emails[row.UserID],
			Value:     row.Value, ResourceExpiresAt: row.ResourceExpiresAt,
			GroupID: row.GroupID, BalanceBefore: row.BalanceBefore, BalanceAfter: row.BalanceAfter,
			CreatedAt: row.CreatedAt,
		}
		if code := row.Edges.Code; code != nil {
			h.Code = code.Code
			h.CodeType = domain.RedemptionType(code.Type)
			h.Remark = code.Remark
		}
		out = append(out, h)
	}
	return out, int64(total), nil
}

// GetUse 取用户对某码的兑换记录；无记录 → ErrNotFound（兑换判定先查 use —— 评审 M-1）。
func (r *RedemptionRepo) GetUse(ctx context.Context, codeID, userID int64) (*domain.RedemptionUse, error) {
	row, err := r.client.RedemptionUse.Query().
		Where(redemptionuse.CodeID(codeID), redemptionuse.UserID(userID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: code_id=%d user_id=%d", ErrNotFound, codeID, userID)
		}
		return nil, err
	}
	return toDomainRedemptionUse(row), nil
}

// CreateUse 插入兑换审计记录；UNIQUE(code_id, user_id) 冲突 → ErrConflict
// （DB 兜底幂等：同用户重复兑换 409 语义）。
func (r *RedemptionRepo) CreateUse(ctx context.Context, use *domain.RedemptionUse) error {
	b := r.client.RedemptionUse.Create().
		SetCodeID(use.CodeID).
		SetUserID(use.UserID).
		SetValue(use.Value).
		SetNillableGroupID(use.GroupID).
		SetNillableBalanceBefore(use.BalanceBefore).
		SetNillableBalanceAfter(use.BalanceAfter)
	if use.ResourceExpiresAt != nil {
		b = b.SetResourceExpiresAt(*use.ResourceExpiresAt)
	}
	if _, err := b.Save(ctx); err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return fmt.Errorf("%w: code_id=%d user_id=%d", ErrConflict, use.CodeID, use.UserID)
		}
		return err
	}
	return nil
}

// IncrementUsed 条件递增 used_count（防并发超卖——评审 I-2）：
// UPDATE redemption_codes SET used_count = used_count + 1
// WHERE id = ? AND used_count < max_uses AND status = active AND
// (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP) —— 单语句条件原子，DB 行锁 + WHERE
// 保证并发兑换最后一张不超卖，并在状态/过期更新与兑换竞态时阻止过期请求。
// 0 行受影响 → (false, nil) = 已用尽/失效（service → 400 并回滚）。
func (r *RedemptionRepo) IncrementUsed(ctx context.Context, codeID int64) (bool, error) {
	u := sql.Update(redemptioncode.Table).
		Set(redemptioncode.FieldUsedCount, sql.ExprFunc(func(b *sql.Builder) {
			b.Ident(redemptioncode.FieldUsedCount).WriteString(" + 1")
		})).
		Where(sql.And(
			sql.EQ(redemptioncode.FieldID, codeID),
			sql.ColumnsLT(redemptioncode.FieldUsedCount, redemptioncode.FieldMaxUses),
			sql.EQ(redemptioncode.FieldStatus, string(redemptioncode.StatusActive)),
			sql.Or(
				sql.IsNull(redemptioncode.FieldExpiresAt),
				sql.GT(redemptioncode.FieldExpiresAt, sql.Expr("CURRENT_TIMESTAMP")),
			),
		))
	n, err := execUpdate(ctx, r.driver, u)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DeactivateCodes 批量失效（单事务）：status → disabled。已 disabled 行 no-op
// （WHERE status <> 'disabled'，不重复计受影响数）；返回受影响行数（新失效数）。
// 缺失 id 由 service 层先查（404 含缺失 id），repo 不报错（评审 M-2：先查后失效
// 窗口竞态可接受——失效不新增行，检查到的 id 不会消失）。空 ids → (0, nil)。
// IN 按 inChunkSize 分片：ids 超 65,535 时单条 UPDATE 超 PG 参数上限（service
// 层已限 ≤100，repo 层自保护）。每块独立 UPDATE，受影响行数累加（块间 id
// 不重叠——分片只按位置切，输入已由 service 去重）。
func (r *RedemptionRepo) DeactivateCodes(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	chunks := chunkIDs(ids, inChunkSize)
	var total int64
	for i, chunk := range chunks {
		n, err := tx.RedemptionCode.Update().
			Where(
				redemptioncode.IDIn(chunk...),
				redemptioncode.StatusNEQ(redemptioncode.StatusDisabled),
			).
			SetStatus(redemptioncode.StatusDisabled).
			Save(ctx)
		if err != nil {
			// 块上下文：任一块失败整体回滚（评审 I-2）
			return 0, fmt.Errorf("deactivate codes (chunk %d/%d, %d ids): %w", i+1, len(chunks), len(chunk), err)
		}
		total += int64(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

func toDomainRedemptionCode(c *ent.RedemptionCode) *domain.RedemptionCode {
	return &domain.RedemptionCode{
		ID: c.ID, Code: c.Code, Type: domain.RedemptionType(c.Type),
		Value: c.Value, GroupID: c.GroupID, Remark: c.Remark,
		ExpiresAt: c.ExpiresAt, ResourceExpiresAt: c.ResourceExpiresAt,
		MaxUses: c.MaxUses, UsedCount: c.UsedCount,
		Status: domain.RedemptionStatus(c.Status), CreatedBy: c.CreatedBy,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

func toDomainRedemptionUse(u *ent.RedemptionUse) *domain.RedemptionUse {
	return &domain.RedemptionUse{
		ID: u.ID, CodeID: u.CodeID, UserID: u.UserID, Value: u.Value,
		ResourceExpiresAt: u.ResourceExpiresAt, GroupID: u.GroupID,
		BalanceBefore: u.BalanceBefore, BalanceAfter: u.BalanceAfter, CreatedAt: u.CreatedAt,
	}
}

// toDomainRedemptionRecord use 行 → 记录视图（码字段由调用方经 WithCode 边填充）。
func toDomainRedemptionRecord(u *ent.RedemptionUse) *domain.RedemptionRecord {
	return &domain.RedemptionRecord{
		ID: u.ID, CodeID: u.CodeID, Value: u.Value,
		ResourceExpiresAt: u.ResourceExpiresAt, GroupID: u.GroupID,
		BalanceBefore: u.BalanceBefore, BalanceAfter: u.BalanceAfter, CreatedAt: u.CreatedAt,
	}
}
