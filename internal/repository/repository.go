// Package repository 用 ent 实现持久化，对外只暴露 domain 类型。
package repository

import (
	"context"

	"entgo.io/ent/dialect"
	"github.com/jackc/pgx/v5/pgxpool"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
)

// Repos 聚合各实体仓库。
type Repos struct {
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
	Client      *ent.Client
}

// New 用既有 driver 构建仓库（PG 生产：entsql.OpenDB(dialect.Postgres, db)；测试：pgxmock 适配器）。
func New(drv dialect.Driver, migrate bool) (*Repos, error) {
	client := ent.NewClient(ent.Driver(drv))
	if migrate {
		if err := client.Schema.Create(context.Background()); err != nil {
			return nil, err
		}
	}
	accounts := &AccountRepo{client: client}
	return &Repos{
		Templates:   &TemplateRepo{client: client},
		Accounts:    accounts,
		Groups:      &GroupRepo{client: client, accounts: accounts},
		Users:       &UserRepo{client: client},
		Keys:        &KeyRepo{client: client},
		Assignments: &GroupAssignmentRepo{client: client},
		Settings:    &SettingRepo{client: client},
		Logs:        &LogRepo{client: client},
		Stats:       &StatRepo{client: client},
		Rules:       &RuleRepo{client: client},
		Client:      client,
	}, nil
}

// --- Store 门面：Repos 聚合全部实体仓库，实现 service.Store（装配入口用）。 ---

func (r *Repos) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	return r.Templates.CreateTemplate(ctx, t)
}

func (r *Repos) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	return r.Templates.GetTemplate(ctx, id)
}

func (r *Repos) ListTemplates(ctx context.Context, q ListQuery) ([]*domain.Template, int64, error) {
	return r.Templates.ListTemplates(ctx, q)
}

func (r *Repos) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	return r.Templates.UpdateTemplate(ctx, t)
}

func (r *Repos) DeleteTemplate(ctx context.Context, id int64) error {
	return r.Templates.DeleteTemplate(ctx, id)
}

func (r *Repos) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	return r.Templates.DeleteTemplatesBatch(ctx, ids)
}

func (r *Repos) UpdateTemplatesBatch(ctx context.Context, ids []int64, p TemplatePatch) error {
	return r.Templates.UpdateTemplatesBatch(ctx, ids, p)
}

func (r *Repos) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	return r.Accounts.CreateAccount(ctx, a)
}

func (r *Repos) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	return r.Accounts.GetAccount(ctx, id)
}

func (r *Repos) ListAccounts(ctx context.Context, q ListQuery) ([]*domain.Account, int64, error) {
	return r.Accounts.ListAccounts(ctx, q)
}

func (r *Repos) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	return r.Accounts.UpdateAccount(ctx, a)
}

func (r *Repos) DeleteAccount(ctx context.Context, id int64) error {
	return r.Accounts.DeleteAccount(ctx, id)
}

func (r *Repos) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	return r.Accounts.DeleteAccountsBatch(ctx, ids)
}

func (r *Repos) UpdateAccountsBatch(ctx context.Context, ids []int64, p AccountPatch) error {
	return r.Accounts.UpdateAccountsBatch(ctx, ids, p)
}

func (r *Repos) SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	return r.Accounts.SetAccountGroups(ctx, accountID, groupIDs)
}

func (r *Repos) GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error) {
	return r.Accounts.GetAccountGroups(ctx, accountID)
}

func (r *Repos) CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	return r.Groups.CreateGroup(ctx, g)
}

func (r *Repos) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	return r.Groups.GetGroup(ctx, id)
}

func (r *Repos) ListGroups(ctx context.Context, q ListQuery) ([]*domain.Group, int64, error) {
	return r.Groups.ListGroups(ctx, q)
}

func (r *Repos) UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	return r.Groups.UpdateGroup(ctx, g)
}

func (r *Repos) DeleteGroup(ctx context.Context, id int64) error {
	return r.Groups.DeleteGroup(ctx, id)
}

func (r *Repos) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	return r.Groups.DeleteGroupsBatch(ctx, ids)
}

func (r *Repos) UpdateGroupsBatch(ctx context.Context, ids []int64, p GroupPatch) error {
	return r.Groups.UpdateGroupsBatch(ctx, ids, p)
}

// --- 用户（Phase 3a） ---

func (r *Repos) CreateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	return r.Users.CreateUser(ctx, u)
}

func (r *Repos) GetUser(ctx context.Context, id int64) (*domain.User, error) {
	return r.Users.GetUser(ctx, id)
}

func (r *Repos) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	return r.Users.GetUserByEmail(ctx, email)
}

func (r *Repos) ListUsers(ctx context.Context, q ListQuery) ([]*domain.User, int64, error) {
	return r.Users.ListUsers(ctx, q)
}

func (r *Repos) UpdateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	return r.Users.UpdateUser(ctx, u)
}

func (r *Repos) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	return r.Users.UpdateUserPassword(ctx, id, passwordHash)
}

// --- 客户端 key（Phase 3a） ---

func (r *Repos) CreateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	return r.Keys.CreateKey(ctx, k)
}

func (r *Repos) GetKey(ctx context.Context, id int64) (*domain.Key, error) {
	return r.Keys.GetKey(ctx, id)
}

func (r *Repos) GetKeyByHash(ctx context.Context, hash string) (*domain.Key, error) {
	return r.Keys.GetKeyByHash(ctx, hash)
}

func (r *Repos) ListKeysByUser(ctx context.Context, userID int64, q ListQuery) ([]*domain.Key, int64, error) {
	return r.Keys.ListKeysByUser(ctx, userID, q)
}

func (r *Repos) UpdateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	return r.Keys.UpdateKey(ctx, k)
}

func (r *Repos) RotateKey(ctx context.Context, id int64, newHash, newPrefix string) (*domain.Key, error) {
	return r.Keys.RotateKey(ctx, id, newHash, newPrefix)
}

func (r *Repos) DeleteKey(ctx context.Context, id int64) error {
	return r.Keys.DeleteKey(ctx, id)
}

func (r *Repos) DeleteKeysByGroup(ctx context.Context, groupID int64) ([]string, error) {
	return r.Keys.DeleteKeysByGroup(ctx, groupID)
}

// --- 组授予（Phase 3a） ---

func (r *Repos) GrantGroup(ctx context.Context, groupID, userID int64) error {
	return r.Assignments.Grant(ctx, groupID, userID)
}

func (r *Repos) RevokeGroup(ctx context.Context, groupID, userID int64) error {
	return r.Assignments.Revoke(ctx, groupID, userID)
}

func (r *Repos) ListAssignmentsByUser(ctx context.Context, userID int64) ([]*domain.GroupAssignment, error) {
	return r.Assignments.ListByUser(ctx, userID)
}

func (r *Repos) ListAssignmentsByGroup(ctx context.Context, groupID int64) ([]*domain.GroupAssignment, error) {
	return r.Assignments.ListByGroup(ctx, groupID)
}

func (r *Repos) ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error) {
	return r.Assignments.ListGroupsForUser(ctx, userID)
}

// --- settings（Phase 3a） ---

func (r *Repos) GetSetting(ctx context.Context, key string) (*domain.Setting, error) {
	return r.Settings.Get(ctx, key)
}

func (r *Repos) GetAllSettings(ctx context.Context) ([]*domain.Setting, error) {
	return r.Settings.GetAll(ctx)
}

func (r *Repos) SetSetting(ctx context.Context, key string, typ domain.SettingType, value string) (*domain.Setting, error) {
	return r.Settings.Set(ctx, key, typ, value)
}

func (r *Repos) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	return r.Rules.ListRules(ctx, enabled)
}

func (r *Repos) CreateRule(ctx context.Context, rl domain.Rule) (int64, error) {
	return r.Rules.CreateRule(ctx, rl)
}

func (r *Repos) UpdateRule(ctx context.Context, rl domain.Rule) error {
	return r.Rules.UpdateRule(ctx, rl)
}

func (r *Repos) DeleteRule(ctx context.Context, id int64) error {
	return r.Rules.DeleteRule(ctx, id)
}

func (r *Repos) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	return r.Rules.DeleteRulesBatch(ctx, ids)
}

func (r *Repos) CountRules(ctx context.Context) (int64, error) {
	return r.Rules.CountRules(ctx)
}

func (r *Repos) QueryLogs(ctx context.Context, q LogQuery) ([]*domain.UsageLog, int64, error) {
	return r.Logs.QueryLogs(ctx, q)
}

func (r *Repos) ScanStats(ctx context.Context, q StatQuery) ([]*domain.StatBucket, error) {
	return r.Stats.ScanStats(ctx, q)
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
