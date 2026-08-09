package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/sqlgraph"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/key"
)

// KeyRepo 客户端 API key（独立表）持久化。
type KeyRepo struct {
	client *ent.Client
	// driver 为 raw SQL（AddQuotaUsed 单语句 CASE 批量更新）用：与 txDriver
	// 组合保证 raw SQL 与 ent 构建器同事务连接（WithTx 同构，评审 I-1）。
	driver dialect.Driver
}

func (r *KeyRepo) CreateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	row, err := r.client.Key.Create().
		SetUserID(k.UserID).
		SetGroupID(k.GroupID).
		SetName(k.Name).
		SetKeyHash(k.KeyHash).
		SetKeyPrefix(k.KeyPrefix).
		SetStatus(key.Status(k.Status)).
		SetMaxConcurrency(k.MaxConcurrency).
		SetQuota(k.Quota).
		SetQuotaUsed(k.QuotaUsed).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: key_hash=%q", ErrConflict, k.KeyHash)
		}
		return nil, err
	}
	return toDomainKey(row), nil
}

func (r *KeyRepo) GetKey(ctx context.Context, id int64) (*domain.Key, error) {
	row, err := r.client.Key.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainKey(row), nil
}

// GetKeyByHash 按 hash 取 key；未找到返回 (nil, nil)。
func (r *KeyRepo) GetKeyByHash(ctx context.Context, hash string) (*domain.Key, error) {
	row, err := r.client.Key.Query().Where(key.KeyHashEQ(hash)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return toDomainKey(row), nil
}

func (r *KeyRepo) ListKeysByUser(ctx context.Context, userID int64, q ListQuery) ([]*domain.Key, int64, error) {
	pred := r.client.Key.Query().Where(key.UserIDEQ(userID))
	if q.Name != "" {
		pred = pred.Where(key.NameContainsFold(q.Name))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(keySortFields)
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
	out := make([]*domain.Key, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainKey(row))
	}
	return out, int64(total), nil
}

// UpdateKey 更新 name/status/max_concurrency/quota/quota_used（hash 走 RotateKey）。
func (r *KeyRepo) UpdateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	row, err := r.client.Key.UpdateOneID(k.ID).
		SetName(k.Name).
		SetStatus(key.Status(k.Status)).
		SetMaxConcurrency(k.MaxConcurrency).
		SetQuota(k.Quota).
		SetQuotaUsed(k.QuotaUsed).
		Save(ctx)
	if err != nil {
		return nil, errMissingID(err, k.ID)
	}
	return toDomainKey(row), nil
}

// RotateKey 轮换 key：换 hash/key_prefix（raw 明文仅服务层返回一次）。
func (r *KeyRepo) RotateKey(ctx context.Context, id int64, newHash, newPrefix string) (*domain.Key, error) {
	row, err := r.client.Key.UpdateOneID(id).SetKeyHash(newHash).SetKeyPrefix(newPrefix).Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: key_hash=%q", ErrConflict, newHash)
		}
		return nil, errMissingID(err, id)
	}
	return toDomainKey(row), nil
}

func (r *KeyRepo) DeleteKey(ctx context.Context, id int64) error {
	if err := r.client.Key.DeleteOneID(id).Exec(ctx); err != nil {
		return errMissingID(err, id)
	}
	return nil
}

// DeleteKeysByGroup 删除组的全部 key，返回被删 hash 列表（Auth 增量清理用；
// 组删除前置清理——key.group_id 外键约束）。
func (r *KeyRepo) DeleteKeysByGroup(ctx context.Context, groupID int64) ([]string, error) {
	rows, err := r.client.Key.Query().
		Where(key.GroupIDEQ(groupID)).
		Select(key.FieldKeyHash).
		All(ctx)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(rows))
	for _, row := range rows {
		hashes = append(hashes, row.KeyHash)
	}
	if _, err := r.client.Key.Delete().Where(key.GroupIDEQ(groupID)).Exec(ctx); err != nil {
		return nil, err
	}
	return hashes, nil
}

// LoadKeys 构建 Auth 鉴权快照：key_hash → KeyMeta（含归属用户状态/并发/
// 额度）。热路径数据源（reload 时一次查询；请求路径零 DB）。
//
// 崩溃修复：旧实现 `WithUser()` eager-load 的 m2o 邻接跳生成
// `SELECT * FROM users WHERE id IN (全部 key 的归属用户 id)`——key 数
// >65,535（用户数也随之超限）即超 PG 参数上限 65535。改为分片：先全表扫
// key id（零参数），按 ≤inChunkSize 切块逐块 `Where(key.IDIn(块))` 加载
// （单块 key ≤8192，其 m2o 邻接 IN ≤8192 个用户 id——每 key 恰一个归属
// 用户，邻接恒被块大小约束）。
func (r *KeyRepo) LoadKeys(ctx context.Context) (map[string]domain.KeyMeta, error) {
	keyIDs, err := r.client.Key.Query().Order(ent.Asc(key.FieldID)).IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("load keys (scan ids): %w", err)
	}
	out := make(map[string]domain.KeyMeta, len(keyIDs))
	chunks := chunkIDs(keyIDs, inChunkSize)
	for i, chunk := range chunks {
		rows, err := r.client.Key.Query().
			Where(key.IDIn(chunk...)).
			WithUser().
			All(ctx)
		if err != nil {
			return nil, fmt.Errorf("load keys (chunk %d/%d, %d ids): %w", i+1, len(chunks), len(chunk), err)
		}
		for _, row := range rows {
			meta := domain.KeyMeta{
				KeyID:      row.ID,
				UserID:     row.UserID,
				GroupID:    row.GroupID,
				KeyStatus:  domain.KeyStatus(row.Status),
				KeyMaxConc: row.MaxConcurrency,
				HasQuota:   row.Quota > 0,
				Quota:      row.Quota,
				QuotaUsed:  row.QuotaUsed,
			}
			if row.Edges.User != nil {
				meta.UserStatus = domain.UserStatus(row.Edges.User.Status)
				meta.UserMaxConc = row.Edges.User.MaxConcurrency
			}
			out[row.KeyHash] = meta
		}
	}
	return out, nil
}

// AddQuotaUsed 批量回写 key 额度消耗（增量；Recorder 节奏，内存权威，
// DB 滞后 ≤ flush 间隔）。单条 SQL CASE 批量更新替代逐 key UpdateOneID 轮询
//（#15 验收：10k 逐 key 额度写回是统计面慢 flush 3-5min 周期根因之一）。
// key 已删（不在 IN 列表）静默跳过——回写无意义（与旧逐 key
// ent.IsNotFound 跳过语义一致）。调用方（usage.Recorder.flushStats）按
// quotaBatchSize 分块并以块为失败回灌原子单位——本方法单语句全成或全败。
func (r *KeyRepo) AddQuotaUsed(ctx context.Context, deltas map[int64]int64) error {
	if len(deltas) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(`UPDATE "keys" SET "quota_used" = "quota_used" + CASE "id" `)
	args := make([]any, 0, len(deltas)*2)
	ids := make([]string, 0, len(deltas))
	for id, d := range deltas {
		if d == 0 {
			continue // 零增量无回写价值
		}
		idx := len(args) + 1
		b.WriteString("WHEN $")
		b.WriteString(strconv.Itoa(idx))
		b.WriteString(" THEN $")
		b.WriteString(strconv.Itoa(idx + 1))
		b.WriteByte(' ')
		args = append(args, id, d)
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	if len(ids) == 0 {
		return nil
	}
	b.WriteString(`ELSE "quota_used" END, "updated_at" = now() WHERE "id" IN (`)
	b.WriteString(strings.Join(ids, ", "))
	b.WriteByte(')')
	var res sql.Result
	return r.driver.Exec(ctx, b.String(), args, &res)
}
