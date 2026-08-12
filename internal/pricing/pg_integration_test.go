// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 真实 PG 集成基座（与 repository 包同款约定）：
//   TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15432/c3api_test \
//     go test ./internal/pricing/ -run PG -v
// 未设置 TEST_DATABASE_URL → t.Skip。每测试重建独立 schema（pricing_test）：
// 与 repository 包的 PG 测试（DROP public schema）隔离——两包测试并发跑不互踩。

// pgTestSchema 本包 PG 测试专用 schema（同一数据库内隔离命名空间）。
const pgTestSchema = "pricing_test"

func newPricingPGRepos(t *testing.T) *repository.Repository {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	// search_path 指向独立 schema：连接池每连接的默认命名空间（pgx runtime
	// param）；ent 迁移与查询按 search_path 解析，不触碰 public。
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + pgTestSchema
	} else {
		dsn += "?search_path=" + pgTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgTestSchema+` CASCADE; CREATE SCHEMA `+pgTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	return repos
}

// TestSyncFlowPG 同步流程走真实 PG：httptest 服务 fixture JSON（真实 HTTP
// 拉取）→ 解析 → UpsertFromLiteLLM（500/批 + manual 行级互斥）→ reload 回调；
// 再同步验证 manual 优先不被覆盖；无效行不落库。
func TestSyncFlowPG(t *testing.T) {
	repos := newPricingPGRepos(t)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(litellmFixtureJSON))
	}))
	defer srv.Close()

	reloads := 0
	w := NewSyncWorker(SyncWorkerConfig{
		Fetcher:  NewFetcher(nil),
		Repo:     repos,
		Settings: &fakeSettings{url: srv.URL, cron: "0 3 * * *"},
		Reload:   func() { reloads++ },
		Log:      nil,
	})
	w.now = fixedNow

	// 首次同步：5 有效行落库
	require.NoError(t, w.Sync(ctx))
	require.Equal(t, 1, reloads, "同步成功后刷新快照")

	got, err := repos.GetPricing(ctx, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(250000), got.PromptPricePerMillion, "2.5e-6 USD/token → 250000 毫分/1M")
	require.Equal(t, int64(1000000), got.CompletionPricePerMillion)
	require.NotNil(t, got.MaxInputTokens)
	require.Equal(t, int64(128000), *got.MaxInputTokens)
	require.Equal(t, domain.PricingSourceLitellm, got.Source)
	// T5/T5b：cache 价 + 元数据 + raw 完整镜像随真实 sync 落库
	require.NotNil(t, got.CacheReadPricePerMillion)
	require.Equal(t, int64(100000), *got.CacheReadPricePerMillion, "cache_read 1e-6 → 100000 毫分/1M")
	require.NotNil(t, got.CacheCreationPricePerMillion)
	require.Equal(t, int64(200000), *got.CacheCreationPricePerMillion)
	require.NotNil(t, got.Provider)
	require.Equal(t, "openai", *got.Provider)
	require.NotNil(t, got.SupportsPromptCaching)
	require.True(t, *got.SupportsPromptCaching)
	require.NotNil(t, got.Raw, "raw 完整镜像落库")
	require.Contains(t, string(got.Raw), `"supports_vision"`, "raw 含未映射字段")

	got, err = repos.GetPricing(ctx, "no-max-tokens")
	require.NoError(t, err)
	require.Nil(t, got.MaxInputTokens, "null/0 max_tokens → nil")
	require.Nil(t, got.CacheReadPricePerMillion, "cache 价 0 → nil")
	require.Nil(t, got.CacheCreationPricePerMillion, "cache 价 null → nil")

	got, err = repos.GetPricing(ctx, "claude-3-5-sonnet")
	require.NoError(t, err)
	require.Nil(t, got.CacheReadPricePerMillion, "cache 价缺失 → nil")
	require.NotNil(t, got.Provider)
	require.Equal(t, "anthropic", *got.Provider)

	// 无效行（0 价/缺 output/负价/字符串/溢出/非对象）不落库
	for _, m := range []string{"zero-cost", "missing-output", "negative-cost",
		"string-cost", "overflow-cost", "not-an-object"} {
		_, err = repos.GetPricing(ctx, m)
		require.ErrorIs(t, err, repository.ErrNotFound, "%s 不应落库", m)
	}

	// 手动价接管 litellm 行后再次同步：WHERE source != 'manual' 过滤 → 手动价不变
	_, err = repos.UpsertManual(ctx, &repository.PricingManual{Model: "gpt-4o", PromptPricePerMillion: 42, CompletionPricePerMillion: 42})
	require.NoError(t, err)
	require.NoError(t, w.Sync(ctx))
	require.Equal(t, 2, reloads)
	got, err = repos.GetPricing(ctx, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(42), got.PromptPricePerMillion, "litellm 同步不覆盖手动价")
	require.Equal(t, domain.PricingSourceManual, got.Source)

	// 删手动价 → 行消失（下轮拉取补回）；再同步 → 恢复 litellm 价
	require.NoError(t, repos.DeleteManual(ctx, "gpt-4o"))
	_, err = repos.GetPricing(ctx, "gpt-4o")
	require.ErrorIs(t, err, repository.ErrNotFound, "删手动价后整行消失（缺失窗口）")
	require.NoError(t, w.Sync(ctx))
	got, err = repos.GetPricing(ctx, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(250000), got.PromptPricePerMillion, "下轮拉取补回 litellm 价")
	require.Equal(t, domain.PricingSourceLitellm, got.Source)
}
