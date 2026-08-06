package repository_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent/account"
	"go-proxy-mini/internal/ent/template"
	"go-proxy-mini/internal/ent/usagelog"
	"go-proxy-mini/internal/repository"
)

// ---------------------------------------------------------------------------
// ent dialect.Driver 桥接：把 pgxmock 池包装成 ent v0.14 的 dialect.Driver
// （Exec/Query 由 driver 侧把结果写入 v；Rows 通过 ColumnScanner 适配）。
// ---------------------------------------------------------------------------

type execQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type mockDriver struct {
	pool pgxmock.PgxPoolIface
}

func (d *mockDriver) Dialect() string { return dialect.Postgres }
func (d *mockDriver) Close() error    { return nil }

func (d *mockDriver) Exec(ctx context.Context, query string, args, v any) error {
	return mockExec(d.pool, ctx, query, args, v)
}

func (d *mockDriver) Query(ctx context.Context, query string, args, v any) error {
	return mockQuery(d.pool, ctx, query, args, v)
}

func (d *mockDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &mockTx{tx: tx, ctx: ctx}, nil
}

type mockTx struct {
	tx  pgx.Tx
	ctx context.Context
}

func (t *mockTx) Exec(ctx context.Context, query string, args, v any) error {
	return mockExec(t.tx, ctx, query, args, v)
}

func (t *mockTx) Query(ctx context.Context, query string, args, v any) error {
	return mockQuery(t.tx, ctx, query, args, v)
}

func (t *mockTx) Commit() error   { return t.tx.Commit(t.ctx) }
func (t *mockTx) Rollback() error { return t.tx.Rollback(t.ctx) }

func mockExec(ex execQuerier, ctx context.Context, query string, args, v any) error {
	argv, ok := args.([]any)
	if !ok {
		return fmt.Errorf("mock driver: invalid args type %T", args)
	}
	tag, err := ex.Exec(ctx, query, argv...)
	if err != nil {
		return err
	}
	if v != nil {
		res, ok := v.(*sql.Result)
		if !ok {
			return fmt.Errorf("mock driver: unexpected Exec target %T", v)
		}
		*res = commandTagResult{tag: tag}
	}
	return nil
}

func mockQuery(ex execQuerier, ctx context.Context, query string, args, v any) error {
	argv, ok := args.([]any)
	if !ok {
		return fmt.Errorf("mock driver: invalid args type %T", args)
	}
	rows, err := ex.Query(ctx, query, argv...)
	if err != nil {
		return err
	}
	vr, ok := v.(*entsql.Rows)
	if !ok {
		return fmt.Errorf("mock driver: unexpected Query target %T", v)
	}
	*vr = entsql.Rows{ColumnScanner: &pgxRowsScanner{rows: rows}}
	return nil
}

// pgxRowsScanner 把 pgx.Rows 适配成 ent 的 ColumnScanner（pgx 没有 Columns()，
// 列名取自 FieldDescriptions）。
type pgxRowsScanner struct {
	rows pgx.Rows
	cols []string
}

func (s *pgxRowsScanner) Columns() ([]string, error) {
	if s.cols == nil {
		for _, fd := range s.rows.FieldDescriptions() {
			s.cols = append(s.cols, fd.Name)
		}
	}
	return s.cols, nil
}

func (s *pgxRowsScanner) ColumnTypes() ([]*sql.ColumnType, error) { return nil, nil }
func (s *pgxRowsScanner) NextResultSet() bool                     { return false }
func (s *pgxRowsScanner) Next() bool                              { return s.rows.Next() }
func (s *pgxRowsScanner) Scan(dest ...any) error                  { return s.rows.Scan(dest...) }
func (s *pgxRowsScanner) Err() error                              { return s.rows.Err() }
func (s *pgxRowsScanner) Close() error {
	s.rows.Close()
	return nil
}

// commandTagResult 把 pgconn.CommandTag 包装成 database/sql.Result。
type commandTagResult struct{ tag pgconn.CommandTag }

func (r commandTagResult) LastInsertId() (int64, error) {
	return 0, errors.New("mock driver: LastInsertId is not supported")
}

func (r commandTagResult) RowsAffected() (int64, error) { return r.tag.RowsAffected(), nil }

// ---------------------------------------------------------------------------
// 测试基座
// ---------------------------------------------------------------------------

type testRepos struct {
	repos *repository.Repos
	pool  pgxmock.PgxPoolIface
}

func newRepos(t *testing.T) *testRepos {
	t.Helper()
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	repos, err := repository.New(&mockDriver{pool: pool}, false)
	require.NoError(t, err)
	return &testRepos{repos: repos, pool: pool}
}

func ctx() context.Context { return context.Background() }

// q 构建一个宽松的 SQL 匹配正则（pgxmock 默认按正则匹配）。
func q(sqlFragment string) string {
	return "(?i)" + regexp.QuoteMeta(sqlFragment)
}

func (tr *testRepos) expectDone(t *testing.T) {
	t.Helper()
	require.NoError(t, tr.pool.ExpectationsWereMet())
}

func templateRow() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"id", "name", "base_url", "default_format", "models",
		"model_formats", "model_mapping", "created_at", "updated_at"}).
		AddRow(int64(1), "openai-main", "https://api.openai.com/v1", "openai-chat",
			[]byte(`["gpt-4o"]`), []byte(`{"o3":"openai-responses"}`),
			[]byte(`{"gpt-4o":"gpt-4o-2026-01-01"}`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
}

func accountRow(status string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"id", "name", "template_id", "upstream_key", "status",
		"cooldown_until", "weight", "max_concurrency", "last_error", "last_used_at",
		"created_at", "updated_at"}).
		AddRow(int64(2), "acc1", int64(1), "sk-x", status, time.Time{}, int64(80), int64(4), "", time.Time{},
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
}

func TestTemplateCRUD(t *testing.T) {
	tr := newRepos(t)

	// Create -> INSERT ... RETURNING id
	tr.pool.ExpectQuery(q(`INSERT INTO "templates"`)).
		WithArgs("openai-main", "https://api.openai.com/v1", template.DefaultFormat("openai-chat"),
			json.RawMessage(`["gpt-4o"]`), json.RawMessage(`{"o3":"openai-responses"}`),
			json.RawMessage(`{"gpt-4o":"gpt-4o-2026-01-01"}`),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)))

	// Get
	tr.pool.ExpectQuery(q(`FROM "templates" WHERE`)).
		WithArgs(int64(1)).
		WillReturnRows(templateRow())

	// Update -> Tx: UPDATE + re-SELECT + Commit
	tr.pool.ExpectBegin()
	tr.pool.ExpectExec(q(`UPDATE "templates" SET`)).
		WithArgs("renamed", "https://api.openai.com/v1", template.DefaultFormat("openai-chat"),
			json.RawMessage(`["gpt-4o"]`), json.RawMessage(`{"o3":"openai-responses"}`),
			json.RawMessage(`{"gpt-4o":"gpt-4o-2026-01-01"}`),
			pgxmock.AnyArg(), int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectQuery(q(`FROM "templates" WHERE`)).
		WithArgs(int64(1)).
		WillReturnRows(templateRow())
	tr.pool.ExpectCommit()

	// Delete
	tr.pool.ExpectExec(q(`DELETE FROM "templates"`)).
		WithArgs(int64(1)).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	// Get after delete -> not found
	tr.pool.ExpectQuery(q(`FROM "templates" WHERE`)).
		WithArgs(int64(1)).
		WillReturnError(pgx.ErrNoRows)

	tpl, err := tr.repos.Templates.CreateTemplate(ctx(), &domain.Template{
		Name:          "openai-main",
		BaseURL:       "https://api.openai.com/v1",
		DefaultFormat: domain.FormatOpenAIChat,
		Models:        []string{"gpt-4o"},
		ModelFormats:  map[string]domain.RequestFormat{"o3": domain.FormatOpenAIResponses},
		ModelMapping:  map[string]string{"gpt-4o": "gpt-4o-2026-01-01"},
	})
	require.NoError(t, err)
	got, err := tr.repos.Templates.GetTemplate(ctx(), tpl.ID)
	require.NoError(t, err)
	require.Equal(t, "openai-main", got.Name)
	require.Equal(t, domain.FormatOpenAIChat, got.DefaultFormat)
	require.Equal(t, domain.FormatOpenAIResponses, got.FormatFor("o3"), "model_formats roundtrip")
	got.Name = "renamed"
	_, err = tr.repos.Templates.UpdateTemplate(ctx(), got)
	require.NoError(t, err)
	require.NoError(t, tr.repos.Templates.DeleteTemplate(ctx(), tpl.ID))
	_, err = tr.repos.Templates.GetTemplate(ctx(), tpl.ID)
	require.Error(t, err, "expected not found after delete")
	tr.expectDone(t)
}

func TestAccountAndGroup(t *testing.T) {
	tr := newRepos(t)

	// Template create
	tr.pool.ExpectQuery(q(`INSERT INTO "templates"`)).
		WithArgs("t", "https://u/v1", template.DefaultFormat("anthropic"),
			json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage(`null`),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)))

	// Account create
	tr.pool.ExpectQuery(q(`INSERT INTO "accounts"`)).
		WithArgs("acc1", "sk-x", account.Status("active"), int(80), int(4),
			pgxmock.AnyArg(), pgxmock.AnyArg(), int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(2)))

	// Group create
	tr.pool.ExpectQuery(q(`INSERT INTO "groups"`)).
		WithArgs("g1", "h1", "gk-aaaa", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(3)))

	// SetAccounts -> Tx: update updated_at + clear M2M + add M2M + re-SELECT + Commit
	tr.pool.ExpectBegin()
	tr.pool.ExpectExec(q(`UPDATE "groups" SET`)).
		WithArgs(pgxmock.AnyArg(), int64(3)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectExec(q(`DELETE FROM "account_groups"`)).
		WithArgs(int64(3)).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	tr.pool.ExpectExec(q(`INSERT INTO "account_groups"`)).
		WithArgs(int64(2), int64(3)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	tr.pool.ExpectQuery(q(`FROM "groups" WHERE`)).
		WithArgs(int64(3)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "key_hash", "key_prefix", "created_at", "updated_at"}).
			AddRow(int64(3), "g1", "h1", "gk-aaaa",
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	tr.pool.ExpectCommit()

	// LoadGroupsAccounts -> groups + accounts(join account_groups) + templates
	tr.pool.ExpectQuery(q(`FROM "groups"`)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "key_hash", "key_prefix", "created_at", "updated_at"}).
			AddRow(int64(3), "g1", "h1", "gk-aaaa",
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	tr.pool.ExpectQuery(q(`JOIN "account_groups"`)).
		WithArgs(int64(3)).
		// 注意：M2M 边加载把 join 列（group_id）放在 SELECT 的第一列。
		WillReturnRows(pgxmock.NewRows([]string{"group_id", "id", "name", "template_id", "upstream_key", "status",
			"cooldown_until", "weight", "max_concurrency", "last_error", "last_used_at",
			"created_at", "updated_at"}).
			AddRow(int64(3), int64(2), "acc1", int64(1), "sk-x", "active", time.Time{}, int64(80), int64(4), "", time.Time{},
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	tr.pool.ExpectQuery(q(`FROM "templates"`)).
		WithArgs(int64(1)).
		WillReturnRows(templateRow())

	// LoadGroupKeys
	tr.pool.ExpectQuery(q(`SELECT "groups"."id"`)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "key_hash"}).AddRow(int64(3), "h1"))

	// UpdateStatus -> Tx: UPDATE + re-SELECT + Commit
	tr.pool.ExpectBegin()
	tr.pool.ExpectExec(q(`UPDATE "accounts" SET`)).
		WithArgs(account.Status("429"), pgxmock.AnyArg(), int64(2)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(2)).
		WillReturnRows(accountRow("429"))
	tr.pool.ExpectCommit()

	// Account Get -> status persisted
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(2)).
		WillReturnRows(accountRow("429"))

	tpl, err := tr.repos.Templates.CreateTemplate(ctx(), &domain.Template{
		Name: "t", BaseURL: "https://u/v1", DefaultFormat: domain.FormatAnthropic,
	})
	require.NoError(t, err)
	acc, err := tr.repos.Accounts.CreateAccount(ctx(), &domain.Account{
		Name: "acc1", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 80, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	g, err := tr.repos.Groups.CreateGroup(ctx(), &domain.Group{Name: "g1", KeyHash: "h1", KeyPrefix: "gk-aaaa"})
	require.NoError(t, err)
	require.NoError(t, tr.repos.Groups.SetGroupAccounts(ctx(), g.ID, []int64{acc.ID}))
	m, err := tr.repos.Groups.LoadGroupsAccounts(ctx())
	require.NoError(t, err)
	got := m[g.ID]
	require.Len(t, got, 1)
	require.Equal(t, acc.ID, got[0].ID)
	require.NotNil(t, got[0].Template)
	keys, err := tr.repos.Groups.LoadGroupKeys(ctx())
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, g.ID, keys["h1"])
	require.NoError(t, tr.repos.Accounts.UpdateAccountStatus(ctx(), acc.ID, domain.Status429, nil, nil))
	a2, err := tr.repos.Accounts.GetAccount(ctx(), acc.ID)
	require.NoError(t, err)
	require.Equal(t, domain.Status429, a2.Status, "status persisted")
	tr.expectDone(t)
}

// listSQL 匹配 List 查询的 SQL 片段（含 ORDER BY/LIMIT/OFFSET 断言）。
func listSQL(order string) string {
	return "(?i)FROM \"templates\".*ORDER BY \"templates\"\\." + order + "( LIMIT \\d+)?( OFFSET \\d+)?"
}

func TestListTemplatesQuery(t *testing.T) {
	// 1) name 模糊 + default_format 筛选：Count 与 List 同条件（PG 下 NameContainsFold → ILIKE）。
	t.Run("filter name and default_format", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectQuery(q(`SELECT COUNT("templates"."id") FROM "templates"`)).
			WithArgs("%main%", template.DefaultFormat("openai-chat")).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))
		tr.pool.ExpectQuery(listSQL(`"id" DESC LIMIT 20`)).
			WithArgs("%main%", template.DefaultFormat("openai-chat")).
			WillReturnRows(pgxmock.NewRows([]string{"id", "name", "base_url", "default_format", "models",
				"model_formats", "model_mapping", "created_at", "updated_at"}).
				AddRow(int64(1), "openai-main", "https://api.openai.com/v1", "openai-chat",
					[]byte(`["gpt-4o"]`), []byte(`{"o3":"openai-responses"}`),
					[]byte(`{"gpt-4o":"gpt-4o-2026-01-01"}`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).
				AddRow(int64(2), "openai-alt", "https://api.openai.com/v1", "openai-chat",
					[]byte(`["gpt-4o-mini"]`), []byte(`{}`), []byte(`{}`),
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

		rows, total, err := tr.repos.Templates.ListTemplates(ctx(), repository.ListQuery{
			Name: "main", DefaultFormat: "openai-chat",
		})
		require.NoError(t, err)
		require.Equal(t, int64(2), total, "Count 与 List 同条件")
		require.Len(t, rows, 2)
		require.Equal(t, "openai-main", rows[0].Name)
		tr.expectDone(t)
	})

	// 2) 分页 + 排序：Sort=name Order=asc → ORDER BY name ASC，Limit=50 Offset=20 内联。
	t.Run("pagination and sort", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectQuery(q(`SELECT COUNT("templates"."id") FROM "templates"`)).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))
		tr.pool.ExpectQuery(`(?i)FROM "templates".*ORDER BY "templates"\."name" ASC LIMIT 50 OFFSET 20`).
			WillReturnRows(pgxmock.NewRows([]string{"id", "name", "base_url", "default_format", "models",
				"model_formats", "model_mapping", "created_at", "updated_at"}).
				AddRow(int64(2), "openai-alt", "https://api.openai.com/v1", "openai-chat",
					[]byte(`["gpt-4o-mini"]`), []byte(`{}`), []byte(`{}`),
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).
				AddRow(int64(1), "openai-main", "https://api.openai.com/v1", "openai-chat",
					[]byte(`["gpt-4o"]`), []byte(`{"o3":"openai-responses"}`),
					[]byte(`{"gpt-4o":"gpt-4o-2026-01-01"}`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

		rows, total, err := tr.repos.Templates.ListTemplates(ctx(), repository.ListQuery{
			Sort: "name", Order: "asc", Offset: 20, Limit: 50,
		})
		require.NoError(t, err)
		require.Equal(t, int64(2), total)
		require.Len(t, rows, 2)
		require.Equal(t, "openai-alt", rows[0].Name, "mock 行序即返回序")
		tr.expectDone(t)
	})

	// 3) 非法 sort → ErrInvalidSort（Count 已执行，List 不执行）。
	t.Run("invalid sort rejected", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectQuery(q(`SELECT COUNT("templates"."id") FROM "templates"`)).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(3)))
		_, _, err := tr.repos.Templates.ListTemplates(ctx(), repository.ListQuery{Sort: "bogus"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "sort")
		tr.expectDone(t)
	})
}

func TestLogsAndStats(t *testing.T) {
	tr := newRepos(t)

	// InsertBatch -> 批量 INSERT ... RETURNING id
	tr.pool.ExpectQuery(q(`INSERT INTO "usage_logs"`)).
		WithArgs(int64(2), int64(0), pgxmock.AnyArg(), "none", usagelog.Format("openai-chat"), int64(1), int64(10),
			"m", int64(0), "r1", int(200), int64(3), int64(100),
			int64(2), int64(0), pgxmock.AnyArg(), "5xx", usagelog.Format("openai-chat"), int64(1), int64(20),
			"m", int64(0), "r2", int(500), int64(3), int64(0)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))

	// Log Query -> Count + SELECT
	tr.pool.ExpectQuery(q(`SELECT COUNT(`)).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))
	tr.pool.ExpectQuery(q(`FROM "usage_logs"`)).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "request_id", "group_id", "account_id", "template_id",
			"model", "mapped_model", "format", "status_code", "error_type", "latency_ms",
			"prompt_tokens", "completion_tokens", "total_tokens", "created_at"}).
			AddRow(int64(1), "r1", int64(1), int64(2), int64(3), "m", "", "openai-chat",
				int(200), "none", int64(10), int64(0), int64(0), int64(100),
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).
			AddRow(int64(2), "r2", int64(1), int64(2), int64(3), "m", "", "openai-chat",
				int(500), "5xx", int64(20), int64(0), int64(0), int64(0),
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

	// Stats Upsert x2 -> INSERT ... ON CONFLICT ... DO UPDATE ... RETURNING id
	//（DO UPDATE 的 COALESCE 增量以 $14.. 追加为独立参数）
	tr.pool.ExpectQuery(q(`INSERT INTO "usage_stats"`)).
		WithArgs(pgxmock.AnyArg(), int64(1), int64(0), int64(0), "m", false,
			int64(2), int64(1), int64(0), int64(0), int64(100), int64(30), pgxmock.AnyArg(),
			int64(2), int64(1), int64(0), int64(0), int64(100), int64(30)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)))
	tr.pool.ExpectQuery(q(`INSERT INTO "usage_stats"`)).
		WithArgs(pgxmock.AnyArg(), int64(1), int64(0), int64(0), "m", false,
			int64(3), int64(1), int64(0), int64(0), int64(200), int64(40), pgxmock.AnyArg(),
			int64(3), int64(1), int64(0), int64(0), int64(200), int64(40)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(2)))

	// Stats Scan（测试未过滤 group_id，仅 bucket_time 范围两个参数）
	tr.pool.ExpectQuery(q(`FROM "usage_stats"`)).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucket_time", "group_id", "account_id", "template_id",
			"model", "is_error", "request_count", "error_count", "prompt_tokens",
			"completion_tokens", "total_tokens", "total_latency_ms", "updated_at"}).
			AddRow(int64(1), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), int64(1), int64(0), int64(0),
				"m", false, int64(5), int64(1), int64(0), int64(0), int64(300), int64(30),
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

	logs := []*domain.UsageLog{
		{RequestID: "r1", GroupID: 1, AccountID: 2, TemplateID: 3, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, LatencyMS: 10, TotalTokens: 100},
		{RequestID: "r2", GroupID: 1, AccountID: 2, TemplateID: 3, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 500, ErrorType: domain.Err5xx, LatencyMS: 20, TotalTokens: 0},
	}
	require.NoError(t, tr.repos.Logs.InsertBatch(ctx(), logs))
	rows, total, err := tr.repos.Logs.QueryLogs(ctx(), repository.LogQuery{GroupID: 1, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, rows, 2)
	bucket := time.Now().Truncate(time.Hour)
	require.NoError(t, tr.repos.Stats.Upsert(ctx(), []*domain.StatBucket{
		{BucketTime: bucket, GroupID: 1, Model: "m", RequestCount: 2, ErrorCount: 1, TotalTokens: 100, TotalLatencyMS: 30},
	}))
	require.NoError(t, tr.repos.Stats.Upsert(ctx(), []*domain.StatBucket{
		{BucketTime: bucket, GroupID: 1, Model: "m", RequestCount: 3, ErrorCount: 1, TotalTokens: 200, TotalLatencyMS: 40},
	}))
	scanned, err := tr.repos.Stats.ScanStats(ctx(), repository.StatQuery{From: bucket, To: bucket.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, scanned, 1)
	require.Equal(t, int64(5), scanned[0].RequestCount, "upsert accumulates")
	require.Equal(t, int64(300), scanned[0].TotalTokens)
	tr.expectDone(t)
}
