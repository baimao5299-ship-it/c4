// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/groupassignment"
)

// GroupRepo 同时承担调度器 Loader 的账号状态回写（UpdateAccountStatus 委托 AccountRepo，
// 由 repository.New 注入；调度器按单个 loader 对象获取数据源）。
type GroupRepo struct {
	client   *ent.Client
	accounts *AccountRepo
	// driver 为成员关系全表扫描用（LoadGroupsAccounts；与 user_repo 同构——
	// 普通 client 与 tx client 均可用）。
	driver dialect.Driver
}

// accountGroupsMembershipSQL 全量成员关系（account_id, group_id）扫描。
// 零 IN 参数——见 LoadGroupsAccounts 注释（ent m2m 跳查询的 IN 上限问题）。
const accountGroupsMembershipSQL = `SELECT account_id, group_id FROM account_groups`

func (r *GroupRepo) CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	// price_multiplier 恒写入（service 层把缺省归一为 10000 = ×1——T3.5 修正：
	// API 边界 nullable float64 可表达显式 0 = 免费组，repo 不再把 0 当"未指定"
	// 跳过落列）。DB 默认 10000 为兜底。
	q := r.client.Group.Create().
		SetName(g.Name).
		SetVisibility(group.Visibility(g.Visibility)).
		SetPriceMultiplier(g.PriceMultiplier).
		// protocol_convert 恒写入（service 层把缺省归一为 "off"；DB 默认 "off" 兜底）。
		SetProtocolConvert(string(g.ProtocolConvert))
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
	// 软删除：列表默认过滤已删（count 同谓词——pred 复用）；GET 单个不过滤。
	pred := r.client.Group.Query().Where(group.DeletedAtIsNil())
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
		SetProtocolConvert(string(g.ProtocolConvert)).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, g.Name)
		}
		return nil, err
	}
	return toDomainGroup(row), nil
}

// DeleteGroup 软删除：deleted_at 置值（行保留留审计；调度器快照按
// deleted_at IS NULL 过滤，GET 单个仍可查已删项）。bulk Update（无 re-SELECT）
// 单语句；0 行命中 = 缺 id → ErrNotFound（与 errMissingID 同格式）。
func (r *GroupRepo) DeleteGroup(ctx context.Context, id int64) error {
	n, err := r.client.Group.Update().Where(group.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return nil
}

// LoadGroupsAccounts 全量组→账号快照（调度器启动/定时/全量失效的数据源）。
//
// 崩溃修复（O3 实证：847k 组 → 启动 fatal / 运行中静默空结果）：ent
// WithAccounts eager-load 对 m2m 边生成两跳参数化 IN——
//  1. `SELECT account_id, group_id FROM account_groups WHERE group_id IN (全部组 id)`
//  2. `SELECT * FROM accounts WHERE id IN (跳1的账号 id)`
//
// 跳1 参数数 = 组实体数，超过 PG 上限 65535（错误 54001 "too many parameters"）
// 即崩溃；且 ent 邻接跳恒为参数化 IN（无法经分片控制），故本方法弃用 eager-load，
// 改为**全表扫描 + 内存 join**（任务决策：语义允许时改 JOIN）：
//  1. `Account.Query().WithTemplate().All`——账号全表扫描；模板 IN 参数数受
//     模板表实体数约束（管理面小表，O3 压测仅 6 个），非账号规模驱动。
//     模板侧嵌套 WithExt（template_ext 1:1 边缘表）——W4 快照合并
//     StripImageTools 用；ext 的 IN 参数数同为模板实体数约束（同一小表界）。
//  2. `Group.Query().IDs`——组 id 全表扫描（零参数；为无账号组保留空条目——
//     与旧 eager-load 语义一致，调度器 Select 区分"组不存在"与"组无账号"）。
//  3. `SELECT account_id, group_id FROM account_groups`——成员关系全表扫描，
//     零参数。
//
// 三条语句参数数都与实体数量无关，任何规模（组/账号 >65,535）都不会触顶。
// 结果语义与旧 eager-load 一致：所有组都在 map 中（无账号组为空切片），
// 账号只出现在其所属组且带模板。（唯一窗口差异：组 id 扫描后被并发删除的
// 组不会出现在结果中——见下方白名单守卫；旧 eager-load 同窗口行为。）
func (r *GroupRepo) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	// 软删除：已删 account/group 不进调度器快照（成员关系白名单守卫同语义）。
	// 模板侧嵌套 WithExt：快照合并 StripImageTools（W4；ext IN 参数数受模板
	// 表实体数约束——同一小表界，见上方注释）。
	accs, err := r.client.Account.Query().Where(account.DeletedAtIsNil()).
		WithTemplate(func(q *ent.TemplateQuery) { q.WithExt() }).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load groups-accounts (accounts scan): %w", err)
	}
	byID := make(map[int64]*domain.Account, len(accs))
	for _, a := range accs {
		byID[a.ID] = toDomainAccount(a)
	}
	gids, err := r.client.Group.Query().Where(group.DeletedAtIsNil()).IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("load groups-accounts (groups scan): %w", err)
	}
	out := make(map[int64][]*domain.Account, len(gids))
	for _, gid := range gids {
		out[gid] = nil // 无账号组保留空条目（与旧 eager-load 同语义）
	}
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, accountGroupsMembershipSQL, []any{}, rows); err != nil {
		return nil, fmt.Errorf("load groups-accounts (membership scan): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var accountID, groupID int64
		if err := rows.Scan(&accountID, &groupID); err != nil {
			return nil, fmt.Errorf("load groups-accounts (membership scan): %w", err)
		}
		a, ok := byID[accountID]
		if !ok {
			continue // 成员关系引用已删账号：忽略（与 eager-load 同语义）
		}
		if _, ok := out[groupID]; !ok {
			// 白名单守卫：组 id 扫描后被并发删除的组不留幽灵条目——
			// 否则 buildSnapshots 会把幽灵组建成真实组，且其账号的共享
			// 实例可能以幽灵组为 gid（与 eager-load 同语义：邻接查询按
			// 已取组 id 过滤，未知父组被 ent 丢弃）。
			continue
		}
		out[groupID] = append(out[groupID], a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load groups-accounts (membership scan): %w", err)
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

// LoadAssignmentMultipliers 全量用户-组专属倍率快照（(user_id, group_id) →
// 万分数；仅 group_assignments.price_multiplier 非 NULL 行——缺失 = 未设置 →
// 用组倍率；billing.Balances.Reload/ReloadMultipliers 调用，T3.5 修正：用户
// 专属倍率按组挂载）。
func (r *GroupRepo) LoadAssignmentMultipliers(ctx context.Context) (map[billing.AssignmentKey]int, error) {
	rows, err := r.client.GroupAssignment.Query().
		Where(groupassignment.PriceMultiplierNotNil()).
		Select(groupassignment.FieldUserID, groupassignment.FieldGroupID, groupassignment.FieldPriceMultiplier).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[billing.AssignmentKey]int, len(rows))
	for _, row := range rows {
		if row.PriceMultiplier == nil {
			continue // 非 NULL 谓词兜底（Select 子集查询仍可能带 nil？不——谓词已过滤，防御）
		}
		out[billing.AssignmentKey{UserID: row.UserID, GroupID: row.GroupID}] = *row.PriceMultiplier
	}
	return out, nil
}

// LoadGroupAccounts 单组账号（组级定向重载数据源）。崩溃修复：旧实现
// `QueryAccounts()` 的 m2m 邻接跳生成 `WHERE id IN (该组全部账号 id)`——
// 单组账号数 >65,535 即超 PG 参数上限。改用 EXISTS 谓词
// `HasGroupsWith(group.IDEQ(groupID))`：ent 生成
// `WHERE accounts.id IN (SELECT ag.account_id FROM account_groups ag JOIN
// groups g ON ag.group_id = g.id WHERE g.id = $1)`——外层 IN 是子查询
// （非参数列表），语句参数数恒为 1（+ 模板 IN，受模板表实体数约束）。
// 返回无序（调用方构建快照/路由，不依赖顺序）。
func (r *GroupRepo) LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error) {
	// 软删除：已删 account 不进调度器快照（与 LoadGroupsAccounts 同语义）。
	accs, err := r.client.Account.Query().
		Where(account.DeletedAtIsNil(), account.HasGroupsWith(group.IDEQ(groupID))).
		WithTemplate(func(q *ent.TemplateQuery) { q.WithExt() }). // W4：ext 边快照合并 StripImageTools
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load group accounts (group %d): %w", groupID, err)
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
