// Package repository 用 ent 实现持久化，对外只暴露 domain 类型。
package repository

import (
	"context"

	"entgo.io/ent/dialect"
	"github.com/jackc/pgx/v5/pgxpool"

	"go-proxy-mini/internal/ent"
)

// Repos 聚合各实体仓库。
type Repos struct {
	Templates *TemplateRepo
	Accounts  *AccountRepo
	Groups    *GroupRepo
	Logs      *LogRepo
	Stats     *StatRepo
	Client    *ent.Client
}

// New 用既有 driver 构建仓库（PG 生产：entsql.OpenDB(dialect.Postgres, db)；测试：pgxmock 适配器）。
func New(drv dialect.Driver, migrate bool) (*Repos, error) {
	client := ent.NewClient(ent.Driver(drv))
	if migrate {
		if err := client.Schema.Create(context.Background()); err != nil {
			return nil, err
		}
	}
	return &Repos{
		Templates: &TemplateRepo{client: client},
		Accounts:  &AccountRepo{client: client},
		Groups:    &GroupRepo{client: client},
		Logs:      &LogRepo{client: client},
		Stats:     &StatRepo{client: client},
		Client:    client,
	}, nil
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
