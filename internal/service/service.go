// Package service 实现管理端业务逻辑：CRUD 校验 + 变更后失效调度/客户端缓存。
package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

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
	RuleStore
	LogStore
	StatStore
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

// KeyRegistrar 由 proxy.Auth 实现，供分组 key 变更时刷新。
type KeyRegistrar interface {
	Upsert(hash string, groupID int64)
	Delete(hash string)
}

type Service struct {
	store      Store
	sched      RuntimeProvider
	invalidate func() // 调度快照失效（全量重载）
	ruleReload RuleReloader
	keys       KeyRegistrar
	log        *logx.Logger
}

func New(store Store, sched RuntimeProvider, invalidate func(), ruleReload RuleReloader, keys KeyRegistrar, log *logx.Logger) *Service {
	return &Service{store: store, sched: sched, invalidate: invalidate, ruleReload: ruleReload, keys: keys, log: log}
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
	u, err := url.Parse(t.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ErrInvalidInput
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
		u, err := url.Parse(*p.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return ErrInvalidInput
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

// mapRepoErr 批量存储错误映射：repository.ErrNotFound → ErrNotFound（保留缺失 id
// 详情，404 响应带 "id=5 missing"）。其他错误原样返回。
func mapRepoErr(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		detail := strings.TrimPrefix(err.Error(), repository.ErrNotFound.Error()+": ")
		return fmt.Errorf("%w: %s", ErrNotFound, detail)
	}
	return err
}
