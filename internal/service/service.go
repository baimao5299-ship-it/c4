// Package service 实现管理端业务逻辑：CRUD 校验 + 变更后失效调度/客户端缓存。
package service

import (
	"context"
	"errors"
	"net/url"
	"slices"

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
	ListTemplates(ctx context.Context) ([]*domain.Template, error)
	UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error)
	DeleteTemplate(ctx context.Context, id int64) error
}

type AccountStore interface {
	CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error)
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	ListAccounts(ctx context.Context) ([]*domain.Account, error)
	UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error)
	DeleteAccount(ctx context.Context, id int64) error
}

type GroupStore interface {
	CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error)
	GetGroup(ctx context.Context, id int64) (*domain.Group, error)
	ListGroups(ctx context.Context) ([]*domain.Group, error)
	UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error)
	DeleteGroup(ctx context.Context, id int64) error
	SetGroupAccounts(ctx context.Context, groupID int64, accountIDs []int64) error
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
