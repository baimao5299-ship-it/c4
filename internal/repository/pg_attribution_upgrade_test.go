// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

var usageAttributionUpgradeColumns = []string{
	"client_ip_source",
	"client_ip_trusted",
	"target_kind",
	"upstream_id",
	"upstream_name",
	"upstream_host",
	"upstream_multiplier_bp",
	"upstream_cost",
	"gross_profit",
	"profit_margin_bp",
}

var errAttributionUpgradeColumns = []string{
	"client_ip_source",
	"client_ip_trusted",
	"target_kind",
	"upstream_id",
	"upstream_name",
	"upstream_host",
	"upstream_multiplier_bp",
}

// TestPGAttributionUpgradeExistingPartitions starts from partitioned parent
// tables that predate upstream attribution. The additive bootstrap must alter
// both parents and already-attached historical partitions without rewriting or
// deleting historical rows. TEST_DATABASE_URL is handled by pgTestPool.
func TestPGAttributionUpgradeExistingPartitions(t *testing.T) {
	ctx := context.Background()
	pool := pgTestPool(t)

	pgExec(t, pool, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public`)
	pgExec(t, pool, oldUsageLogsDDL)
	pgExec(t, pool, oldErrLogsDDL)
	pgExec(t, pool, `CREATE TABLE usage_logs_20240102 PARTITION OF usage_logs
		FOR VALUES FROM ('2024-01-02 00:00:00+00') TO ('2024-01-03 00:00:00+00')`)
	pgExec(t, pool, `CREATE TABLE err_logs_20240102 PARTITION OF err_logs
		FOR VALUES FROM ('2024-01-02 00:00:00+00') TO ('2024-01-03 00:00:00+00')`)
	pgExec(t, pool, `INSERT INTO usage_logs (
		id, request_id, client_ip, model, format, error_type, cost, raw_cost, created_at
	) VALUES (11, 'old-usage', '198.51.100.11', 'old-model', 'openai-chat', 'none', 700, 500, '2024-01-02 12:00:00+00')`)
	pgExec(t, pool, `INSERT INTO err_logs (
		id, request_id, client_ip, model, format, status_code, error_type, error_message, created_at
	) VALUES (21, 'old-error', '198.51.100.21', 'old-model', 'openai-chat', 502, '5xx', 'old failure', '2024-01-02 13:00:00+00')`)

	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), false)
	require.NoError(t, err)

	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for pass := 1; pass <= 2; pass++ {
		require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, now), "usage upgrade pass %d", pass)
		require.NoError(t, repos.EnsureErrLogPartitioned(ctx, now), "error upgrade pass %d", pass)
		requireAttributionUpgradeState(t, pool)
	}
}

func requireAttributionUpgradeState(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"usage_logs", "usage_logs_20240102"} {
		requireNullableColumns(t, pool, table, usageAttributionUpgradeColumns)
	}
	for _, table := range []string{"err_logs", "err_logs_20240102"} {
		requireNullableColumns(t, pool, table, errAttributionUpgradeColumns)
	}

	var (
		usageRequestID          string
		usageCost, usageRawCost int64
		clientIPSource          sql.NullString
		clientIPTrusted         sql.NullBool
		targetKind              sql.NullString
		upstreamID              sql.NullInt64
		upstreamName            sql.NullString
		upstreamHost            sql.NullString
		upstreamMultiplier      sql.NullInt64
		upstreamCost            sql.NullInt64
		grossProfit             sql.NullInt64
		profitMargin            sql.NullInt64
	)
	err := pool.QueryRow(context.Background(), `SELECT
		request_id, cost, raw_cost, client_ip_source, client_ip_trusted, target_kind,
		upstream_id, upstream_name, upstream_host, upstream_multiplier_bp,
		upstream_cost, gross_profit, profit_margin_bp
		FROM usage_logs WHERE id = 11`).Scan(
		&usageRequestID, &usageCost, &usageRawCost, &clientIPSource, &clientIPTrusted,
		&targetKind, &upstreamID, &upstreamName, &upstreamHost, &upstreamMultiplier,
		&upstreamCost, &grossProfit, &profitMargin,
	)
	require.NoError(t, err)
	require.Equal(t, "old-usage", usageRequestID)
	require.Equal(t, int64(700), usageCost)
	require.Equal(t, int64(500), usageRawCost)
	require.False(t, clientIPSource.Valid)
	require.False(t, clientIPTrusted.Valid)
	require.False(t, targetKind.Valid)
	require.False(t, upstreamID.Valid)
	require.False(t, upstreamName.Valid)
	require.False(t, upstreamHost.Valid)
	require.False(t, upstreamMultiplier.Valid)
	require.False(t, upstreamCost.Valid)
	require.False(t, grossProfit.Valid)
	require.False(t, profitMargin.Valid)

	var (
		errRequestID       string
		errStatusCode      int
		errClientIPSource  sql.NullString
		errClientIPTrusted sql.NullBool
		errTargetKind      sql.NullString
		errUpstreamID      sql.NullInt64
		errUpstreamName    sql.NullString
		errUpstreamHost    sql.NullString
		errMultiplier      sql.NullInt64
	)
	err = pool.QueryRow(context.Background(), `SELECT
		request_id, status_code, client_ip_source, client_ip_trusted, target_kind,
		upstream_id, upstream_name, upstream_host, upstream_multiplier_bp
		FROM err_logs WHERE id = 21`).Scan(
		&errRequestID, &errStatusCode, &errClientIPSource, &errClientIPTrusted,
		&errTargetKind, &errUpstreamID, &errUpstreamName, &errUpstreamHost, &errMultiplier,
	)
	require.NoError(t, err)
	require.Equal(t, "old-error", errRequestID)
	require.Equal(t, 502, errStatusCode)
	require.False(t, errClientIPSource.Valid)
	require.False(t, errClientIPTrusted.Valid)
	require.False(t, errTargetKind.Valid)
	require.False(t, errUpstreamID.Valid)
	require.False(t, errUpstreamName.Valid)
	require.False(t, errUpstreamHost.Valid)
	require.False(t, errMultiplier.Valid)

	require.Equal(t, int64(1), pgCount(t, pool, `SELECT count(*) FROM usage_logs_20240102`))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT count(*) FROM err_logs_20240102`))
}

func requireNullableColumns(t *testing.T, pool *pgxpool.Pool, table string, want []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT column_name, is_nullable
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1
			AND column_name = ANY($2::text[])`, table, want)
	require.NoError(t, err)
	defer rows.Close()

	found := make(map[string]string, len(want))
	for rows.Next() {
		var column, nullable string
		require.NoError(t, rows.Scan(&column, &nullable))
		found[column] = nullable
	}
	require.NoError(t, rows.Err())
	require.Len(t, found, len(want), "%s must contain every additive column", table)
	for _, column := range want {
		require.Equal(t, "YES", found[column], "%s.%s must preserve unknown as NULL", table, column)
	}
}

const oldUsageLogsDDL = `CREATE TABLE usage_logs (
	id bigint NOT NULL,
	request_id varchar NOT NULL,
	client_ip text NULL,
	group_id bigint NULL,
	account_id bigint NULL,
	template_id bigint NULL,
	user_id bigint NULL,
	key_id bigint NULL,
	model varchar NOT NULL DEFAULT '',
	mapped_model varchar NULL,
	format varchar NOT NULL,
	error_type varchar NOT NULL DEFAULT 'none',
	latency_ms bigint NOT NULL DEFAULT 0,
	ttft_ms bigint NULL,
	input_tokens bigint NOT NULL DEFAULT 0,
	price_input_millis bigint NULL,
	output_tokens bigint NOT NULL DEFAULT 0,
	price_output_millis bigint NULL,
	total_tokens bigint NOT NULL DEFAULT 0,
	cache_read_tokens bigint NOT NULL DEFAULT 0,
	price_cache_read_millis bigint NULL,
	cache_creation_tokens bigint NOT NULL DEFAULT 0,
	price_cache_creation_millis bigint NULL,
	call_count bigint NOT NULL DEFAULT 0,
	price_per_call_millis bigint NULL,
	cost bigint NOT NULL DEFAULT 0,
	raw_cost bigint NOT NULL DEFAULT 0,
	billing_tier varchar NULL,
	above_hit boolean NOT NULL DEFAULT false,
	overdraft boolean NOT NULL DEFAULT false,
	billed boolean NOT NULL DEFAULT false,
	created_at timestamptz NOT NULL,
	PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at)`

const oldErrLogsDDL = `CREATE TABLE err_logs (
	id bigint NOT NULL,
	request_id varchar NOT NULL,
	client_ip text NULL,
	group_id bigint NULL,
	account_id bigint NULL,
	template_id bigint NULL,
	user_id bigint NULL,
	key_id bigint NULL,
	model varchar NOT NULL DEFAULT '',
	format varchar NOT NULL,
	status_code integer NOT NULL DEFAULT 0,
	error_type varchar NOT NULL DEFAULT 'none',
	error_message varchar NULL,
	latency_ms bigint NOT NULL DEFAULT 0,
	billing_tier varchar NULL,
	created_at timestamptz NOT NULL,
	PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at)`
