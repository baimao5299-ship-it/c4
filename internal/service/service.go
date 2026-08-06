// Package service 实现管理端业务逻辑：CRUD 校验 + 变更后失效调度/客户端缓存。
package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/pkg/logx"
)

var (
	ErrNotFound     = errors.New("service: not found")
	ErrInvalidInput = errors.New("service: invalid input")
)

type Store interface {
	TemplateStore
	AccountStore
	GroupStore
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
}

type GroupStore interface {
	CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error)
	GetGroup(ctx context.Context, id int64) (*domain.Group, error)
	ListGroups(ctx context.Context, q repository.ListQuery) ([]*domain.Group, int64, error)
	UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error)
	DeleteGroup(ctx context.Context, id int64) error
	SetGroupAccounts(ctx context.Context, groupID int64, accountIDs []int64) error
	DeleteGroupsBatch(ctx context.Context, ids []int64) error
	UpdateGroupsBatch(ctx context.Context, ids []int64, p repository.GroupPatch) error
}

type LogStore interface {
	QueryLogs(ctx context.Context, q repository.LogQuery) ([]*domain.UsageLog, int64, error)
}

type StatStore interface {
	ScanStats(ctx context.Context, q repository.StatQuery) ([]*domain.StatBucket, error)
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
	keys       KeyRegistrar
	log        *logx.Logger
}

func New(store Store, sched RuntimeProvider, invalidate func(), keys KeyRegistrar, log *logx.Logger) *Service {
	return &Service{store: store, sched: sched, invalidate: invalidate, keys: keys, log: log}
}

func validateTemplate(t *domain.Template) error {
	if t.Name == "" {
		return ErrInvalidInput
	}
	u, err := url.Parse(t.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ErrInvalidInput
	}
	if !t.DefaultFormat.Valid() {
		return ErrInvalidInput
	}
	for _, f := range t.ModelFormats {
		if !f.Valid() {
			return ErrInvalidInput
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
	"templates": {"id", "name", "base_url", "default_format", "created_at", "updated_at"},
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
	if p.DefaultFormat != nil && !p.DefaultFormat.Valid() {
		return ErrInvalidInput
	}
	if p.ModelFormats != nil {
		for _, f := range *p.ModelFormats {
			if !f.Valid() {
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
