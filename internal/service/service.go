// Package service 实现管理端业务逻辑：CRUD 校验 + 变更后失效调度/客户端缓存。
package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"go-proxy-mini/internal/credential"
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/pkg/logx"
)

var (
	ErrNotFound     = errors.New("service: not found")
	ErrInvalidInput = errors.New("service: invalid input")
	ErrConflict     = errors.New("service: conflict")
)

type Store interface {
	TemplateStore
	AccountStore
	GroupStore
	KeyStore
	GroupAssignmentStore
	UserStore
	SettingStore
	RuleStore
	LogStore
	StatStore
	RedemptionStore
	// WithTx 在单事务内执行 fn（评审 I-1）：真实仓库为 tx 版 Repository（全部走
	// tx 连接）；fake 为事务语义模拟（fn 内变更先入暂存、成功提交/失败丢弃——
	// 回滚断言的前提）。
	WithTx(ctx context.Context, fn func(repository.TxStore) error) error
}

// UserStore 用户持久化（Phase 3a）。
type UserStore interface {
	CreateUser(ctx context.Context, u *domain.User) (*domain.User, error)
	GetUser(ctx context.Context, id int64) (*domain.User, error)
	GetUserByEmail(ctx context.Context, email string) (*domain.User, error)
	ListUsers(ctx context.Context, q repository.ListQuery) ([]*domain.User, int64, error)
	UpdateUser(ctx context.Context, u *domain.User) (*domain.User, error)
	UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error
	// 原子资源更新（评审 I-1：兑换码 applier 用；普通 client 与 tx client 均可用）。
	UpdateUserBalance(ctx context.Context, userID, delta int64) error
	UpdateUserMaxConcurrency(ctx context.Context, userID int64, value int) error
	// CreateTempBalance 创建临时额度行（注册赠品、兑换码兑换等；user_id 外键必
	// 存在）。expiresAt/note 为 nil 时不落该列（nil = 永久；兑换码路径必非零）。
	CreateTempBalance(ctx context.Context, userID int64, amount int64, expiresAt *time.Time, note *string) error
}

// SettingStore 类型化配置持久化（Phase 3a）。
type SettingStore interface {
	GetSetting(ctx context.Context, key string) (*domain.Setting, error)
	GetAllSettings(ctx context.Context) ([]*domain.Setting, error)
	SetSetting(ctx context.Context, key string, typ domain.SettingType, value string) (*domain.Setting, error)
}

// KeyStore 客户端 key 持久化（/user/keys 面 + 组删除前置清理）。
type KeyStore interface {
	CreateKey(ctx context.Context, k *domain.Key) (*domain.Key, error)
	GetKey(ctx context.Context, id int64) (*domain.Key, error)
	ListKeysByUser(ctx context.Context, userID int64, q repository.ListQuery) ([]*domain.Key, int64, error)
	UpdateKey(ctx context.Context, k *domain.Key) (*domain.Key, error)
	RotateKey(ctx context.Context, id int64, newHash, newPrefix string) (*domain.Key, error)
	DeleteKey(ctx context.Context, id int64) error
	// DeleteKeysByGroup 组删除前置清理（key.group_id 外键约束；返回被删 hash）。
	DeleteKeysByGroup(ctx context.Context, groupID int64) ([]string, error)
}

// GroupAssignmentStore private 组授予持久化（/admin/groups/{id}/assignments +
// /user/groups 可选组列表）。
type GroupAssignmentStore interface {
	GrantGroup(ctx context.Context, groupID, userID int64) error
	RevokeGroup(ctx context.Context, groupID, userID int64) error
	ListAssignmentsByUser(ctx context.Context, userID int64) ([]*domain.GroupAssignment, error)
	ListAssignmentsByGroup(ctx context.Context, groupID int64) ([]*domain.GroupAssignment, error)
	ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error)
}

type TemplateStore interface {
	CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error)
	GetTemplate(ctx context.Context, id int64) (*domain.Template, error)
	ListTemplates(ctx context.Context, q repository.ListQuery) ([]*domain.Template, int64, error)
	UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error)
	DeleteTemplate(ctx context.Context, id int64) error
	DeleteTemplatesBatch(ctx context.Context, ids []int64) error
	UpdateTemplatesBatch(ctx context.Context, ids []int64, p repository.TemplatePatch) error
}

type AccountStore interface {
	CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error)
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	ListAccounts(ctx context.Context, q repository.ListQuery) ([]*domain.Account, int64, error)
	UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error)
	DeleteAccount(ctx context.Context, id int64) error
	DeleteAccountsBatch(ctx context.Context, ids []int64) error
	UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) error
	// SetAccountGroups 替换账号的全部分组（替换语义；空数组 = 清空）。
	SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error
	// GetAccountGroups 账号的分组 id 列表（编辑回显；账号缺 id 由调用方先
	// GetAccount 拦截）。
	GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error)
}

type GroupStore interface {
	CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error)
	GetGroup(ctx context.Context, id int64) (*domain.Group, error)
	ListGroups(ctx context.Context, q repository.ListQuery) ([]*domain.Group, int64, error)
	UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error)
	DeleteGroup(ctx context.Context, id int64) error
	DeleteGroupsBatch(ctx context.Context, ids []int64) error
	UpdateGroupsBatch(ctx context.Context, ids []int64, p repository.GroupPatch) error
}

// RedemptionStore 兑换码 + 兑换审计持久化（Phase 5 计费前基础设施）。
// 兑换事务编排（Redeem）经 Store.WithTx 以 repository.TxStore 面访问。
type RedemptionStore interface {
	CreateCodes(ctx context.Context, codes []*domain.RedemptionCode) error
	GetByCode(ctx context.Context, code string) (*domain.RedemptionCode, error)
	GetCode(ctx context.Context, id int64) (*domain.RedemptionCode, error)
	ListCodes(ctx context.Context, q repository.ListQuery, typ *domain.RedemptionType, status *domain.RedemptionStatus) ([]*domain.RedemptionCode, int64, error)
	ListCodeUses(ctx context.Context, codeID int64, q repository.ListQuery) ([]*domain.RedemptionUse, int64, error)
	// ListUsesByUser 某用户的兑换记录（/user/redemptions；use + 码联查视图）。
	ListUsesByUser(ctx context.Context, userID int64, q repository.ListQuery) ([]*domain.RedemptionRecord, int64, error)
	DeactivateCodes(ctx context.Context, ids []int64) (int64, error)
	GetUse(ctx context.Context, codeID, userID int64) (*domain.RedemptionUse, error)
	CreateUse(ctx context.Context, use *domain.RedemptionUse) error
	IncrementUsed(ctx context.Context, codeID int64) (bool, error)
}

type LogStore interface {
	QueryLogs(ctx context.Context, q repository.LogQuery) ([]*domain.UsageLog, int64, error)
}

type StatStore interface {
	ScanStats(ctx context.Context, q repository.StatQuery) ([]*domain.StatBucket, error)
}

// RuleReloader 由 rule.RuleEngine 实现：规则 CRUD 后全量重载（invalidate 钩子）。
// 独立于通用 invalidate——规则重载会重置窗口计数，不能随任意资源变更触发。
type RuleReloader interface {
	Reload(ctx context.Context) error
}

// RuntimeProvider 由 scheduler 实现，供账号运行时视图。
type RuntimeProvider interface {
	Runtime(accountID int64) (scheduler.RuntimeInfo, bool)
}

// KeyRegistrar 由 proxy.Auth 实现，供客户端 key 变更时增量刷新鉴权快照。
type KeyRegistrar interface {
	Upsert(hash string, meta domain.KeyMeta)
	Delete(hash string)
}

type Service struct {
	store      Store
	sched      RuntimeProvider
	invalidate func() // 调度快照失效（全量重载）
	ruleReload RuleReloader
	keys       KeyRegistrar
	// settings 设置全量内存快照（默认值 + DB 覆盖）：公开读路径（注册等）
	// 零 DB 直读；仅管理面 UpdateSetting 后重载（低频，无锁）。
	settings atomic.Pointer[map[string]*domain.Setting]
	log      *logx.Logger
}

func New(store Store, sched RuntimeProvider, invalidate func(), ruleReload RuleReloader, keys KeyRegistrar, log *logx.Logger) *Service {
	s := &Service{store: store, sched: sched, invalidate: invalidate, ruleReload: ruleReload, keys: keys, log: log}
	s.reloadSettings(context.Background())
	return s
}

// validateBaseURL 校验 base_url：可解析、有 scheme/host，且为裸根（不含尾
// /v1）。/v1 是协议细节（aiclient 按格式追加；anthropic SDK 自带 v1 前缀，
// base 含 /v1 会拼出 /v1/v1/messages 404）——约定裸根，防呆拒绝含 /v1。
func validateBaseURL(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ErrInvalidInput
	}
	if strings.HasSuffix(strings.TrimSuffix(base, "/"), "/v1") {
		return ErrInvalidInput
	}
	return nil
}

func validateTemplate(t *domain.Template) error {
	// 评审 M-1：默认值兜底在 service 层——repo 全字段 Set 会原样写空串，
	// handler 直传也可能缺省；空/缺省在此归一为 api_key，随后才校验合法性。
	if t.CredentialType == "" {
		t.CredentialType = credential.TypeAPIKey
	}
	if !t.CredentialType.Valid() {
		return ErrInvalidInput
	}
	if t.Name == "" {
		return ErrInvalidInput
	}
	if err := validateBaseURL(t.BaseURL); err != nil {
		return err
	}
	if len(t.SupportedFormats) == 0 {
		return ErrInvalidInput
	}
	seen := make(map[domain.RequestFormat]bool, len(t.SupportedFormats))
	for _, f := range t.SupportedFormats {
		if !f.Valid() || seen[f] {
			return ErrInvalidInput
		}
		seen[f] = true
	}
	for f, models := range t.FormatModels {
		if !seen[f] || len(models) == 0 {
			return ErrInvalidInput
		}
		for _, m := range models {
			// 模型必须在可服务集合（排除 format_models 自身，防自引用循环）
			if !slices.Contains(t.Models, m) {
				if _, ok := t.ModelMapping[m]; !ok {
					return ErrInvalidInput
				}
			}
		}
	}
	return nil
}

func validateAccount(a *domain.Account) error {
	if a.Name == "" || a.UpstreamKey == "" || a.TemplateID <= 0 {
		return ErrInvalidInput
	}
	if a.Weight < 0 {
		return ErrInvalidInput
	}
	if a.MaxConcurrency < 1 {
		a.MaxConcurrency = 8
	}
	return nil
}

// listSortFields 各资源允许的 sort 白名单（与 repo 层白名单一致，双保险）。
var listSortFields = map[string][]string{
	"templates": {"id", "name", "base_url", "created_at", "updated_at"},
	"accounts":  {"id", "name", "template_id", "status", "cooldown_until", "weight", "max_concurrency", "last_used_at", "created_at", "updated_at"},
	"groups":    {"id", "name", "created_at", "updated_at"},
	"users":     {"id", "email", "role", "status", "max_concurrency", "created_at", "updated_at"},
	"keys":      {"id", "name", "status", "max_concurrency", "quota", "quota_used", "created_at", "updated_at"},
	// 与 repo 层 redemptionCodeSortFields 白名单一致（双保险）。
	"redemption_codes": {"id", "code", "type", "value", "max_uses", "used_count", "status", "created_by", "created_at", "updated_at"},
	// 与 repo 层 redemptionUseSortFields 白名单一致（双保险；/user/redemptions）。
	"redemption_uses": {"id", "code_id", "user_id", "value", "created_at"},
}

// validateListQuery sort/order 白名单校验（非法 → ErrInvalidInput；handler 依赖此 400）。
func validateListQuery(q repository.ListQuery, sortFields []string) error {
	if q.Order != "" && q.Order != "asc" && q.Order != "desc" {
		return ErrInvalidInput
	}
	if q.Sort != "" && !slices.Contains(sortFields, q.Sort) {
		return ErrInvalidInput
	}
	return nil
}

// --- 批量操作校验与错误映射 ---

// validateIDs ids 1–100 且去重（handler 已做，service 兜底）。
func validateIDs(ids []int64) error {
	if len(ids) == 0 || len(ids) > 100 {
		return ErrInvalidInput
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			return ErrInvalidInput
		}
		seen[id] = struct{}{}
	}
	return nil
}

// validateTemplatePatch 校验批量 patch 提供的字段（nil = 未提供，跳过）。
// 多格式语义与 validateTemplate 对齐：supported_formats 非空/枚举/去重；
// format_models 的 key 必须合法枚举且列表非空；两者同批提供时 key 必须
// ∈ supported_formats（跨字段子集校验，与单 PUT 的 validateTemplate 一致）。
func validateTemplatePatch(p repository.TemplatePatch) error {
	if p.Name != nil && *p.Name == "" {
		return ErrInvalidInput
	}
	if p.BaseURL != nil {
		if err := validateBaseURL(*p.BaseURL); err != nil {
			return err
		}
	}
	var supported map[domain.RequestFormat]bool
	if p.SupportedFormats != nil {
		if len(*p.SupportedFormats) == 0 {
			return ErrInvalidInput
		}
		supported = make(map[domain.RequestFormat]bool, len(*p.SupportedFormats))
		for _, f := range *p.SupportedFormats {
			if !f.Valid() || supported[f] {
				return ErrInvalidInput
			}
			supported[f] = true
		}
	}
	if p.FormatModels != nil {
		for f, models := range *p.FormatModels {
			if !f.Valid() || len(models) == 0 {
				return ErrInvalidInput
			}
			if supported != nil && !supported[f] {
				return ErrInvalidInput
			}
		}
	}
	return nil
}

// validateAccountPatch 校验批量 patch 提供的字段（nil = 未提供，跳过）。
func validateAccountPatch(p repository.AccountPatch) error {
	if p.Name != nil && *p.Name == "" {
		return ErrInvalidInput
	}
	if p.UpstreamKey != nil && *p.UpstreamKey == "" {
		return ErrInvalidInput
	}
	if p.TemplateID != nil && *p.TemplateID <= 0 {
		return ErrInvalidInput
	}
	if p.Weight != nil && *p.Weight < 0 {
		return ErrInvalidInput
	}
	if p.MaxConcurrency != nil && *p.MaxConcurrency < 1 {
		return ErrInvalidInput
	}
	// GroupIDs：nil/空数组合法（nil = 不变，[] = 清空）；非空要求长度 ≤ 100、
	// 去重、元素 > 0（与 template_id 对齐：非法 id 值在 service 层拦截为 400，
	// 不落到 repo 层变 404 语义）。
	if p.GroupIDs != nil {
		if len(*p.GroupIDs) > 100 {
			return ErrInvalidInput
		}
		seen := make(map[int64]struct{}, len(*p.GroupIDs))
		for _, id := range *p.GroupIDs {
			if id <= 0 {
				return ErrInvalidInput
			}
			if _, ok := seen[id]; ok {
				return ErrInvalidInput
			}
			seen[id] = struct{}{}
		}
	}
	return nil
}

// mapRepoErr 存储错误映射：repository.ErrNotFound → ErrNotFound（保留缺失 id
// 详情，404 响应带 "id=5 missing"）；repository.ErrConflict → ErrConflict
// （保留冲突详情，409 响应带 "name=\"x\""）。其他错误原样返回。
func mapRepoErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		detail := strings.TrimPrefix(err.Error(), repository.ErrNotFound.Error()+": ")
		return fmt.Errorf("%w: %s", ErrNotFound, detail)
	case errors.Is(err, repository.ErrConflict):
		detail := strings.TrimPrefix(err.Error(), repository.ErrConflict.Error()+": ")
		return fmt.Errorf("%w: %s", ErrConflict, detail)
	}
	return err
}
