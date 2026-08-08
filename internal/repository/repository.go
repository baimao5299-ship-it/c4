// Package repository 用 ent 实现持久化，对外只暴露 domain 类型。
package repository

import (
	"context"
	"database/sql"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
)

// Repository 聚合各实体仓库（绑定单一 client + driver；WithTx 复用同一构造函数
// newRepository 构造 tx 版实例）。
type Repository struct {
	Templates   *TemplateRepo
	Accounts    *AccountRepo
	Groups      *GroupRepo
	Users       *UserRepo
	Keys        *KeyRepo
	Assignments *GroupAssignmentRepo
	Settings    *SettingRepo
	Logs        *LogRepo
	Stats       *StatRepo
	Rules       RuleStore
	Redemptions *RedemptionRepo
	Pricing     *PricingRepo
	Billing     *BillingRepo // 扣费落库（Phase 5 T3）
	Client      *ent.Client
	// driver 为原始 dialect.Driver：原子资源方法/条件递增等 raw SQL 走它
	//（ent v0.14 生成代码无 ExecContext/QueryContext，raw SQL 无客户端入口）；
	// WithTx 内为事务驱动（txDriver），保证 raw SQL 与 ent 构建器同连接。
	driver dialect.Driver
}

// New 用既有 driver 构建仓库（PG 生产：entsql.OpenDB(dialect.Postgres, db)；测试：pgxmock 适配器）。
func New(drv dialect.Driver, migrate bool) (*Repository, error) {
	client := ent.NewClient(ent.Driver(drv))
	if migrate {
		if err := client.Schema.Create(context.Background()); err != nil {
			return nil, err
		}
	}
	return newRepository(client, drv), nil
}

// newRepository 用给定 client/driver 构建全量仓库（New 与 WithTx 复用同一构造函数；
// WithTx 注入 tx client + 事务驱动，fn 内所有方法调用都走 tx —— 评审 I-1）。
func newRepository(client *ent.Client, drv dialect.Driver) *Repository {
	accounts := &AccountRepo{client: client}
	return &Repository{
		Templates:   &TemplateRepo{client: client},
		Accounts:    accounts,
		Groups:      &GroupRepo{client: client, accounts: accounts},
		Users:       &UserRepo{client: client, driver: drv},
		Keys:        &KeyRepo{client: client},
		Assignments: &GroupAssignmentRepo{client: client},
		Settings:    &SettingRepo{client: client},
		Logs:        &LogRepo{client: client},
		Stats:       &StatRepo{client: client},
		Rules:       &RuleRepo{client: client},
		Redemptions: &RedemptionRepo{client: client, driver: drv},
		Pricing:     &PricingRepo{client: client, driver: drv},
		Billing:     &BillingRepo{client: client, driver: drv},
		Client:      client,
		driver:      drv,
	}
}

// --- 事务（评审 I-1 核心） ---

// txDriver 把 dialect.Tx 包装成 dialect.Driver（镜像 ent 内部 txDriver 语义）：
// ent 构建器与 raw SQL（原子资源方法/IncrementUsed）经同一驱动 → 同一事务连接。
// Commit/Rollback 为 nop，由 WithTx 直接对 dialect.Tx 提交/回滚。
type txDriver struct {
	drv dialect.Driver
	tx  dialect.Tx
}

func (d *txDriver) Dialect() string                        { return d.drv.Dialect() }
func (d *txDriver) Close() error                           { return nil }
func (d *txDriver) Tx(context.Context) (dialect.Tx, error) { return d, nil }
func (d *txDriver) Commit() error                          { return nil }
func (d *txDriver) Rollback() error                        { return nil }
func (d *txDriver) Exec(ctx context.Context, query string, args, v any) error {
	return d.tx.Exec(ctx, query, args, v)
}
func (d *txDriver) Query(ctx context.Context, query string, args, v any) error {
	return d.tx.Query(ctx, query, args, v)
}

// TxStore WithTx 事务回调面（评审 I-1）：兑换编排/测试在单事务内仅经此面访问
// 资源更新与兑换数据。*Repository 实现（WithTx 注入 tx 版实例，全部走同一事务
// 连接）；service 层 fake 实现同面做事务语义模拟（评审 I-1 回滚断言的前提——
// 回调参数因此用接口而非 *Repository，同时约束 applier 无法绕过 tx 面）。
// 面内任一步失败 → 整体回滚。
type TxStore interface {
	CreateCodes(ctx context.Context, codes []*domain.RedemptionCode) error
	GetUse(ctx context.Context, codeID, userID int64) (*domain.RedemptionUse, error)
	GetByCode(ctx context.Context, code string) (*domain.RedemptionCode, error)
	UpdateUserBalance(ctx context.Context, userID, delta int64) error
	UpdateUserMaxConcurrency(ctx context.Context, userID int64, value int) error
	CreateTempBalance(ctx context.Context, userID int64, amount int64, expiresAt *time.Time, note *string) error
	CreateUse(ctx context.Context, use *domain.RedemptionUse) error
	IncrementUsed(ctx context.Context, codeID int64) (bool, error)
}

// WithTx 在单事务内执行 fn（评审 I-1）：ent `Tx().Client()` 模式构造 tx 版 Repository
// （复用 newRepository，注入 tx client + 事务驱动），fn 内所有方法调用（含原子资源
// 方法）都走 tx；fn 返回错误 → 整体回滚，nil → Commit。兑换编排（Task 2）用：
// applier 必须只经 tx 面调资源更新，任一步失败（含 use 冲突/计数用尽）全部回滚。
func (r *Repository) WithTx(ctx context.Context, fn func(TxStore) error) error {
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	drv := &txDriver{tx: tx, drv: r.driver}
	tr := newRepository(ent.NewClient(ent.Driver(drv)), drv)
	if err := fn(tr); err != nil {
		return err
	}
	return tx.Commit()
}

// execUpdate 执行 UPDATE 构建器，返回受影响行数（raw SQL 统一入口；
// dialect.Driver.Exec 的 v 形参为 *sql.Result，见 pgxmock 测试适配器同款）。
// 注意：裸 sql.Update 构建器无方言信息（默认反引号引号），执行前按驱动方言设置
// （PG → 双引号），否则 Postgres 报语法错误。
func execUpdate(ctx context.Context, drv dialect.Driver, u *entsql.UpdateBuilder) (int64, error) {
	u.SetDialect(drv.Dialect())
	query, args := u.Query()
	var res sql.Result
	if err := drv.Exec(ctx, query, args, &res); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- Store 门面：Repository 聚合全部实体仓库，实现 service.Store（装配入口用）。 ---

func (r *Repository) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	return r.Templates.CreateTemplate(ctx, t)
}

func (r *Repository) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	return r.Templates.GetTemplate(ctx, id)
}

func (r *Repository) ListTemplates(ctx context.Context, q ListQuery) ([]*domain.Template, int64, error) {
	return r.Templates.ListTemplates(ctx, q)
}

func (r *Repository) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	return r.Templates.UpdateTemplate(ctx, t)
}

func (r *Repository) DeleteTemplate(ctx context.Context, id int64) error {
	return r.Templates.DeleteTemplate(ctx, id)
}

func (r *Repository) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	return r.Templates.DeleteTemplatesBatch(ctx, ids)
}

func (r *Repository) UpdateTemplatesBatch(ctx context.Context, ids []int64, p TemplatePatch) error {
	return r.Templates.UpdateTemplatesBatch(ctx, ids, p)
}

func (r *Repository) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	return r.Accounts.CreateAccount(ctx, a)
}

func (r *Repository) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	return r.Accounts.GetAccount(ctx, id)
}

func (r *Repository) ListAccounts(ctx context.Context, q ListQuery) ([]*domain.Account, int64, error) {
	return r.Accounts.ListAccounts(ctx, q)
}

func (r *Repository) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	return r.Accounts.UpdateAccount(ctx, a)
}

func (r *Repository) DeleteAccount(ctx context.Context, id int64) error {
	return r.Accounts.DeleteAccount(ctx, id)
}

func (r *Repository) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	return r.Accounts.DeleteAccountsBatch(ctx, ids)
}

func (r *Repository) UpdateAccountsBatch(ctx context.Context, ids []int64, p AccountPatch) error {
	return r.Accounts.UpdateAccountsBatch(ctx, ids, p)
}

func (r *Repository) SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	return r.Accounts.SetAccountGroups(ctx, accountID, groupIDs)
}

func (r *Repository) GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error) {
	return r.Accounts.GetAccountGroups(ctx, accountID)
}

func (r *Repository) CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	return r.Groups.CreateGroup(ctx, g)
}

func (r *Repository) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	return r.Groups.GetGroup(ctx, id)
}

func (r *Repository) ListGroups(ctx context.Context, q ListQuery) ([]*domain.Group, int64, error) {
	return r.Groups.ListGroups(ctx, q)
}

func (r *Repository) UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	return r.Groups.UpdateGroup(ctx, g)
}

func (r *Repository) DeleteGroup(ctx context.Context, id int64) error {
	return r.Groups.DeleteGroup(ctx, id)
}

func (r *Repository) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	return r.Groups.DeleteGroupsBatch(ctx, ids)
}

func (r *Repository) UpdateGroupsBatch(ctx context.Context, ids []int64, p GroupPatch) error {
	return r.Groups.UpdateGroupsBatch(ctx, ids, p)
}

// --- 用户（Phase 3a） ---

func (r *Repository) CreateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	return r.Users.CreateUser(ctx, u)
}

func (r *Repository) GetUser(ctx context.Context, id int64) (*domain.User, error) {
	return r.Users.GetUser(ctx, id)
}

func (r *Repository) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	return r.Users.GetUserByEmail(ctx, email)
}

func (r *Repository) ListUsers(ctx context.Context, q ListQuery) ([]*domain.User, int64, error) {
	return r.Users.ListUsers(ctx, q)
}

func (r *Repository) UpdateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	return r.Users.UpdateUser(ctx, u)
}

func (r *Repository) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	return r.Users.UpdateUserPassword(ctx, id, passwordHash)
}

// CreateTempBalance 临时额度行（注册赠品；UserRepo 扩展方法，不新增独立
// repo 字段——避免与并行任务的门面改动冲突）。
func (r *Repository) CreateTempBalance(ctx context.Context, userID int64, amount int64, expiresAt *time.Time, note *string) error {
	return r.Users.CreateTempBalance(ctx, userID, amount, expiresAt, note)
}

// --- 客户端 key（Phase 3a） ---

func (r *Repository) CreateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	return r.Keys.CreateKey(ctx, k)
}

func (r *Repository) GetKey(ctx context.Context, id int64) (*domain.Key, error) {
	return r.Keys.GetKey(ctx, id)
}

func (r *Repository) GetKeyByHash(ctx context.Context, hash string) (*domain.Key, error) {
	return r.Keys.GetKeyByHash(ctx, hash)
}

func (r *Repository) ListKeysByUser(ctx context.Context, userID int64, q ListQuery) ([]*domain.Key, int64, error) {
	return r.Keys.ListKeysByUser(ctx, userID, q)
}

func (r *Repository) UpdateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	return r.Keys.UpdateKey(ctx, k)
}

func (r *Repository) RotateKey(ctx context.Context, id int64, newHash, newPrefix string) (*domain.Key, error) {
	return r.Keys.RotateKey(ctx, id, newHash, newPrefix)
}

func (r *Repository) DeleteKey(ctx context.Context, id int64) error {
	return r.Keys.DeleteKey(ctx, id)
}

func (r *Repository) DeleteKeysByGroup(ctx context.Context, groupID int64) ([]string, error) {
	return r.Keys.DeleteKeysByGroup(ctx, groupID)
}

// --- 组授予（Phase 3a） ---

func (r *Repository) GrantGroup(ctx context.Context, groupID, userID int64) error {
	return r.Assignments.Grant(ctx, groupID, userID)
}

func (r *Repository) RevokeGroup(ctx context.Context, groupID, userID int64) error {
	return r.Assignments.Revoke(ctx, groupID, userID)
}

func (r *Repository) ListAssignmentsByUser(ctx context.Context, userID int64) ([]*domain.GroupAssignment, error) {
	return r.Assignments.ListByUser(ctx, userID)
}

func (r *Repository) ListAssignmentsByGroup(ctx context.Context, groupID int64) ([]*domain.GroupAssignment, error) {
	return r.Assignments.ListByGroup(ctx, groupID)
}

func (r *Repository) ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error) {
	return r.Assignments.ListGroupsForUser(ctx, userID)
}

// --- settings（Phase 3a） ---

func (r *Repository) GetSetting(ctx context.Context, key string) (*domain.Setting, error) {
	return r.Settings.Get(ctx, key)
}

func (r *Repository) GetAllSettings(ctx context.Context) ([]*domain.Setting, error) {
	return r.Settings.GetAll(ctx)
}

func (r *Repository) SetSetting(ctx context.Context, key string, typ domain.SettingType, value string) (*domain.Setting, error) {
	return r.Settings.Set(ctx, key, typ, value)
}

func (r *Repository) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	return r.Rules.ListRules(ctx, enabled)
}

func (r *Repository) CreateRule(ctx context.Context, rl domain.Rule) (int64, error) {
	return r.Rules.CreateRule(ctx, rl)
}

func (r *Repository) UpdateRule(ctx context.Context, rl domain.Rule) error {
	return r.Rules.UpdateRule(ctx, rl)
}

func (r *Repository) DeleteRule(ctx context.Context, id int64) error {
	return r.Rules.DeleteRule(ctx, id)
}

func (r *Repository) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	return r.Rules.DeleteRulesBatch(ctx, ids)
}

func (r *Repository) CountRules(ctx context.Context) (int64, error) {
	return r.Rules.CountRules(ctx)
}

func (r *Repository) QueryLogs(ctx context.Context, q LogQuery) ([]*domain.UsageLog, int64, error) {
	return r.Logs.QueryLogs(ctx, q)
}

func (r *Repository) ScanStats(ctx context.Context, q StatQuery) ([]*domain.StatBucket, error) {
	return r.Stats.ScanStats(ctx, q)
}

// --- 兑换码（Phase 5 计费前基础设施） ---

func (r *Repository) CreateCodes(ctx context.Context, codes []*domain.RedemptionCode) error {
	return r.Redemptions.CreateCodes(ctx, codes)
}

func (r *Repository) GetByCode(ctx context.Context, code string) (*domain.RedemptionCode, error) {
	return r.Redemptions.GetByCode(ctx, code)
}

func (r *Repository) GetCode(ctx context.Context, id int64) (*domain.RedemptionCode, error) {
	return r.Redemptions.GetCode(ctx, id)
}

func (r *Repository) ListCodes(ctx context.Context, q ListQuery, typ *domain.RedemptionType, status *domain.RedemptionStatus) ([]*domain.RedemptionCode, int64, error) {
	return r.Redemptions.ListCodes(ctx, q, typ, status)
}

func (r *Repository) ListCodeUses(ctx context.Context, codeID int64, q ListQuery) ([]*domain.RedemptionUse, int64, error) {
	return r.Redemptions.ListCodeUses(ctx, codeID, q)
}

func (r *Repository) ListUsesByUser(ctx context.Context, userID int64, q ListQuery) ([]*domain.RedemptionRecord, int64, error) {
	return r.Redemptions.ListUsesByUser(ctx, userID, q)
}

func (r *Repository) GetUse(ctx context.Context, codeID, userID int64) (*domain.RedemptionUse, error) {
	return r.Redemptions.GetUse(ctx, codeID, userID)
}

func (r *Repository) CreateUse(ctx context.Context, use *domain.RedemptionUse) error {
	return r.Redemptions.CreateUse(ctx, use)
}

func (r *Repository) IncrementUsed(ctx context.Context, codeID int64) (bool, error) {
	return r.Redemptions.IncrementUsed(ctx, codeID)
}

func (r *Repository) DeactivateCodes(ctx context.Context, ids []int64) (int64, error) {
	return r.Redemptions.DeactivateCodes(ctx, ids)
}

// --- 模型价格（Phase 5 计费价格来源） ---

func (r *Repository) UpsertFromLiteLLM(ctx context.Context, rows []*domain.Pricing) (int, error) {
	return r.Pricing.UpsertFromLiteLLM(ctx, rows)
}

func (r *Repository) UpsertManual(ctx context.Context, m *PricingManual) (*domain.Pricing, error) {
	return r.Pricing.UpsertManual(ctx, m)
}

func (r *Repository) DeleteManual(ctx context.Context, model string) error {
	return r.Pricing.DeleteManual(ctx, model)
}

func (r *Repository) ListPricing(ctx context.Context, q ListQuery, source *domain.PricingSource, model string) ([]*domain.Pricing, int64, error) {
	return r.Pricing.ListPricing(ctx, q, source, model)
}

func (r *Repository) GetPricing(ctx context.Context, model string) (*domain.Pricing, error) {
	return r.Pricing.GetPricing(ctx, model)
}

// --- 原子资源更新（评审 I-1：UserStore 扩展；普通 client 与 tx client 均可用） ---

func (r *Repository) UpdateUserBalance(ctx context.Context, userID, delta int64) error {
	return r.Users.UpdateUserBalance(ctx, userID, delta)
}

// DeductAndLog 批量扣费 + 计费日志落库（Phase 5 T3 计费 flusher 写路径）：
// FEFO 临时额度优先 + 条件扣费（允许透支）+ 同事务批量日志，见 BillingRepo。
func (r *Repository) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (overdrafted bool, balanceAfter int64, err error) {
	return r.Billing.DeductAndLog(ctx, userID, cost, logs)
}

// LoadBalances 全量余额快照（Phase 5 计费余额预检数据源）。
func (r *Repository) LoadBalances(ctx context.Context) (map[int64]int64, error) {
	return r.Users.LoadBalances(ctx)
}

func (r *Repository) UpdateUserMaxConcurrency(ctx context.Context, userID int64, value int) error {
	return r.Users.UpdateUserMaxConcurrency(ctx, userID, value)
}

// OpenPG 打开 pgx 连接池（生产入口；ent driver 由调用方用
// entsql.OpenDB(dialect.Postgres, stdlib.OpenDBFromPool(pool)) 构建）。
func OpenPG(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns
	return pgxpool.NewWithConfig(ctx, cfg)
}
