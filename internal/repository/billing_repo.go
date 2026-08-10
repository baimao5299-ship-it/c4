package repository

import (
	"context"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/tempbalance"
	"go-proxy-mini/internal/ent/user"
)

// usageLogBatchSize 计费日志单批插入行数上限（#37 P2）：ent CreateBulk 参数 =
// 列数 × 行数，超 PG 65535 参数上限即失败（"extended protocol limited to
// 65535 parameters"，19 列 × ~3448 行——压测实证扣费停滞根因）。500 行/批
// （19 × 500 = 9,500 参数），与 usage InsertBatch 分块同量级。
const usageLogBatchSize = 500

// BillingRepo 扣费落库：FEFO 临时额度优先 + 条件扣费（允许透支）+ 同事务批量
// 计费日志。全毫分直接扣减（1 USD = 100,000 毫分，零换算零取整误差）。
type BillingRepo struct {
	client *ent.Client
	// driver 为 raw SQL（FEFO/余额条件更新）用：与 txDriver 组合保证 raw SQL
	// 与 ent 构建器同事务连接（WithTx 同构，评审 I-1）。
	driver dialect.Driver
}

// DeductAndLog 批量扣费 + 计费日志落库（单事务，全成或全败）：
//
// ① FEFO 临时额度（评审 I-4 用户裁决）：未过期 temp_balances 按 expires_at
//
//	升序逐行扣（最早到期先扣、永久 NULL 最后——PG ASC 默认 NULLS LAST），
//	行级条件更新 amount >= take 防并发透支（多实例并发扣同一用户时 SELECT
//	读到的存量可能已被他事务扣减：条件不满足则跳过该行，剩余转扣余额，
//	temp 恒不为负）
//
// ② 剩余扣 users.balance：条件更新 balance >= remain（防并发负余额）；0 行 →
//
//	无条件扣（允许透支）；再 0 行 = user 不存在 → 跳过扣减仍插日志
//	（usagelog 无 FK）
//
// ③ 事务内 SELECT balance 回读（行锁串行无竞态）返回 balanceAfter
// ④ logs 逐个 Overdraft=overdrafted 同事务 CreateBulk 插入
//
// 任一步失败整体回滚。cost == 0 → 只插日志（不扣款；balanceAfter = 当前余额
// 原值，overdrafted=false）。
func (r *BillingRepo) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (overdrafted bool, balanceAfter int64, err error) {
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	drv := &txDriver{tx: tx, drv: r.driver}
	br := &BillingRepo{client: ent.NewClient(ent.Driver(drv)), driver: drv}
	od, bal, err := br.deductAndLogTx(ctx, userID, cost, logs)
	if err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return od, bal, nil
}

// deductAndLogTx 事务内实现（DeductAndLog 与 WithTx 同构复用 txDriver）。
func (r *BillingRepo) deductAndLogTx(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	remain := cost
	if cost > 0 {
		rows, err := r.client.TempBalance.Query().
			Select(tempbalance.FieldID, tempbalance.FieldAmount).
			Where(
				tempbalance.UserIDEQ(userID),
				tempbalance.AmountGT(0),
				tempbalance.Or(tempbalance.ExpiresAtIsNil(), tempbalance.ExpiresAtGT(time.Now())),
			).
			Order(tempbalance.ByExpiresAt()).
			All(ctx)
		if err != nil {
			return false, 0, err
		}
		for _, tb := range rows {
			if remain <= 0 {
				break
			}
			take := tb.Amount
			if take > remain {
				take = remain
			}
			u := sql.Update(tempbalance.Table).
				Set(tempbalance.FieldAmount, sql.ExprFunc(func(b *sql.Builder) {
					b.Ident(tempbalance.FieldAmount).WriteString(" - ").Arg(take)
				})).
				Where(sql.And(
					sql.EQ(tempbalance.FieldID, tb.ID),
					sql.GTE(tempbalance.FieldAmount, take), // 行级防并发透支
				))
			n, err := execUpdate(ctx, r.driver, u)
			if err != nil {
				return false, 0, err
			}
			if n > 0 {
				remain -= take
			}
		}
	}
	overdrafted := false
	userExists := true
	if remain > 0 {
		n, err := execUpdate(ctx, r.driver, sql.Update(user.Table).
			Set(user.FieldBalance, sql.ExprFunc(func(b *sql.Builder) {
				b.Ident(user.FieldBalance).WriteString(" - ").Arg(remain)
			})).
			Where(sql.And(
				sql.EQ(user.FieldID, userID),
				sql.GTE(user.FieldBalance, remain),
			)))
		if err != nil {
			return false, 0, err
		}
		if n == 0 {
			// 余额不足 → 无条件扣（允许透支）；再 0 行 = 用户不存在
			n, err = execUpdate(ctx, r.driver, sql.Update(user.Table).
				Set(user.FieldBalance, sql.ExprFunc(func(b *sql.Builder) {
					b.Ident(user.FieldBalance).WriteString(" - ").Arg(remain)
				})).
				Where(sql.EQ(user.FieldID, userID)))
			if err != nil {
				return false, 0, err
			}
			if n == 0 {
				userExists = false
			} else {
				overdrafted = true
			}
		}
	}
	balanceAfter := int64(0)
	if userExists {
		row, err := r.client.User.Query().Select(user.FieldBalance).Where(user.IDEQ(userID)).Only(ctx)
		if err != nil {
			return false, 0, err
		}
		balanceAfter = row.Balance
	}
	if len(logs) > 0 {
		// 批量插入分片（#37 P2）：ent CreateBulk 参数 = 列数 × 行数，单批超
		// PG 65535 参数上限即失败（"extended protocol limited to 65535
		// parameters"，19 列 × ~3448 行——压测实证：单 user 大批日志 → 扣费
		// 停滞、pending 积压）。≤500 行/批（19 × 500 = 9,500 参数，与 usage
		// InsertBatch 分块同量级），同事务逐片 CreateBulk，任一失败整体回滚
		// 语义不变（chunk 原子性由外层事务保证）。
		for start := 0; start < len(logs); start += usageLogBatchSize {
			end := min(start+usageLogBatchSize, len(logs))
			builders := make([]*ent.UsageLogCreate, 0, end-start)
			for _, l := range logs[start:end] {
				l.Overdraft = overdrafted
				builders = append(builders, buildUsageLogCreate(r.client, l))
			}
			if _, err := r.client.UsageLog.CreateBulk(builders...).Save(ctx); err != nil {
				return false, 0, err
			}
		}
	}
	return overdrafted, balanceAfter, nil
}
