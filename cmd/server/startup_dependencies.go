// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/redisx"
)

const startupDependencyBudget = 30 * time.Second

func openRedisWithRetry(ctx context.Context, cfg config.RedisConfig) (*redis.Client, error) {
	var client *redis.Client
	err := retryStartup(ctx, func() error {
		var err error
		client, err = redisx.Open(redisx.Options{Addr: cfg.Addr, Password: cfg.Password, DB: cfg.DB})
		return err
	}, retryableStartupError, nil)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// bootstrapDatabase retries only the connection/readiness part of startup.
// Each failed attempt closes its pool before the next one, so a database
// restart cannot accumulate stale pools or connections.
func bootstrapDatabase(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, *repository.Repository, string, error) {
	if cfg == nil {
		return nil, nil, "db", fmt.Errorf("database config is not configured")
	}
	var pool *pgxpool.Pool
	var repos *repository.Repository
	step := "db"
	err := retryStartup(ctx, func() error {
		step = "db"
		attemptPool, err := repository.OpenPG(ctx, cfg.DB.DSN, int32(cfg.DB.MaxConns))
		if err != nil {
			return err
		}
		attemptDB := stdlib.OpenDBFromPool(attemptPool)
		attemptDrv := entsql.OpenDB(dialect.Postgres, attemptDB)
		attemptRepos, err := repository.NewWithPG(ctx, attemptDrv, true, attemptPool)
		if err != nil {
			attemptPool.Close()
			step = "migrate"
			return err
		}
		checks := []struct {
			name string
			fn   func(context.Context, time.Time) error
		}{
			{"usagelog partition bootstrap", attemptRepos.EnsureUsageLogPartitioned},
			{"err_logs partition bootstrap", attemptRepos.EnsureErrLogPartitioned},
			{"usage_stats partition bootstrap", attemptRepos.EnsureUsageStatsPartitioned},
			{"usage_entity_stats partition bootstrap", attemptRepos.EnsureUsageEntityStatsPartitioned},
		}
		now := time.Now()
		for _, check := range checks {
			step = check.name
			if err := check.fn(ctx, now); err != nil {
				attemptPool.Close()
				return err
			}
		}
		step = "price_variants effect check bootstrap"
		if err := attemptRepos.EnsurePriceVariantsEffectCheck(ctx); err != nil {
			attemptPool.Close()
			return err
		}
		step = "codex-search price seed bootstrap"
		if err := attemptRepos.EnsureCodexSearchSeed(ctx); err != nil {
			attemptPool.Close()
			return err
		}
		pool = attemptPool
		repos = attemptRepos
		return nil
	}, retryableStartupError, nil)
	if err != nil {
		return nil, nil, step, err
	}
	return pool, repos, "", nil
}
