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

	"go-proxy-mini/internal/credential"
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent/account"
	"go-proxy-mini/internal/ent/group"
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
	repos *repository.Repository
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
	return pgxmock.NewRows([]string{"id", "name", "base_url", "credential_type", "supported_formats", "models",
		"format_models", "model_mapping", "created_at", "updated_at"}).
		AddRow(int64(1), "openai-main", "https://api.openai.com/v1", "api_key",
			[]byte(`["openai-chat","openai-responses"]`), []byte(`["gpt-4o"]`),
			[]byte(`{"openai-responses":["o3"]}`),
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

	// Create -> INSERT ... RETURNING id（credential_type 全字段 Set：api_key）
	tr.pool.ExpectQuery(q(`INSERT INTO "templates"`)).
		WithArgs("openai-main", "https://api.openai.com/v1", "api_key",
			json.RawMessage(`["openai-chat","openai-responses"]`), json.RawMessage(`["gpt-4o"]`),
			json.RawMessage(`{"openai-responses":["o3"]}`),
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
		WithArgs("renamed", "https://api.openai.com/v1", "api_key",
			json.RawMessage(`["openai-chat","openai-responses"]`), json.RawMessage(`["gpt-4o"]`),
			json.RawMessage(`{"openai-responses":["o3"]}`),
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
		Name:             "openai-main",
		BaseURL:          "https://api.openai.com/v1",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIResponses: {"o3"}},
		ModelMapping:     map[string]string{"gpt-4o": "gpt-4o-2026-01-01"},
	})
	require.NoError(t, err)
	got, err := tr.repos.Templates.GetTemplate(ctx(), tpl.ID)
	require.NoError(t, err)
	require.Equal(t, "openai-main", got.Name)
	require.Equal(t, credential.TypeAPIKey, got.CredentialType, "credential_type 模板级映射")
	require.ElementsMatch(t, []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses}, got.SupportedFormats)
	require.True(t, got.FormatSupports(domain.FormatOpenAIResponses, "o3"), "format_models roundtrip")
	require.False(t, got.FormatSupports(domain.FormatOpenAIResponses, "gpt-4o"), "responses 配置了 format_models → 仅列表内模型")
	require.True(t, got.FormatSupports(domain.FormatOpenAIChat, "gpt-4o"), "chat 未配置 format_models → 全部模型")
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

	// Template create（repo 全字段 Set：credential_type 空串原样写入——默认值兜底在 service 层）
	tr.pool.ExpectQuery(q(`INSERT INTO "templates"`)).
		WithArgs("t", "https://u/v1", "", json.RawMessage(`["anthropic"]`),
			json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage(`null`),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)))

	// Account create（账号级无 credential_type 字段）
	tr.pool.ExpectQuery(q(`INSERT INTO "accounts"`)).
		WithArgs("acc1", "sk-x", account.Status("active"), int(80), int(4),
			pgxmock.AnyArg(), pgxmock.AnyArg(), int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(2)))

	// Group create（Phase 3a：无 key 字段，visibility 默认 public；
	// price_multiplier 恒写入——T3.5 修正：service 归一缺省为 10000，显式 0 = 免费组）
	tr.pool.ExpectQuery(q(`INSERT INTO "groups"`)).
		WithArgs("g1", group.VisibilityPublic, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(3)))

	// SetAccountGroups -> checkGroupExist 预校验（SELECT groups）+ 自动 Tx（M2M
	// 边变更）：update updated_at + clear M2M + add M2M + re-SELECT + Commit
	//（账号侧绑定，替代已删的 SetGroupAccounts）
	tr.pool.ExpectQuery(q(`SELECT "groups"."id" FROM "groups" WHERE`)).
		WithArgs(int64(3)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(3)))
	tr.pool.ExpectBegin()
	tr.pool.ExpectExec(q(`UPDATE "accounts" SET`)).
		WithArgs(pgxmock.AnyArg(), int64(2)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectExec(q(`DELETE FROM "account_groups"`)).
		WithArgs(int64(2)).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	tr.pool.ExpectExec(q(`INSERT INTO "account_groups"`)).
		WithArgs(int64(2), int64(3)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(2)).
		WillReturnRows(accountRow("active"))
	tr.pool.ExpectCommit()

	// GetAccountGroups -> JOIN (SELECT ... FROM "account_groups" ...) 读分组 id
	tr.pool.ExpectQuery(q(`FROM "account_groups"`)).
		WithArgs(int64(2)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(3)))

	// LoadGroupsAccounts -> accounts 全表 + templates(eager) + groups id 全表
	// + account_groups 全表成员关系（#18：零 IN 参数全扫描，替代 ent
	// eager-load 的 `WHERE group_id IN (全部组 id)`——组数 >65,535 超 PG
	// 参数上限）
	tr.pool.ExpectQuery(q(`FROM "accounts"`)).
		WillReturnRows(accountRow("active"))
	tr.pool.ExpectQuery(q(`FROM "templates"`)).
		WithArgs(int64(1)).
		WillReturnRows(templateRow())
	tr.pool.ExpectQuery(q(`FROM "groups"`)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(3)))
	tr.pool.ExpectQuery(q(`SELECT account_id, group_id FROM account_groups`)).
		WillReturnRows(pgxmock.NewRows([]string{"account_id", "group_id"}).
			AddRow(int64(2), int64(3)))

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
		Name: "t", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic},
	})
	require.NoError(t, err)
	acc, err := tr.repos.Accounts.CreateAccount(ctx(), &domain.Account{
		Name: "acc1", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 80, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	g, err := tr.repos.Groups.CreateGroup(ctx(), &domain.Group{Name: "g1", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	require.NoError(t, tr.repos.Accounts.SetAccountGroups(ctx(), acc.ID, []int64{g.ID}))
	gIDs, err := tr.repos.Accounts.GetAccountGroups(ctx(), acc.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{g.ID}, gIDs, "GetAccountGroups round-trip")
	m, err := tr.repos.Groups.LoadGroupsAccounts(ctx())
	require.NoError(t, err)
	got := m[g.ID]
	require.Len(t, got, 1)
	require.Equal(t, acc.ID, got[0].ID)
	require.NotNil(t, got[0].Template)
	// Phase 3a：LoadGroupKeys 已删除（key 独立表；LoadKeys 覆盖见真实 PG 测试
	// pg_auth_keys_test.go）
	require.NoError(t, tr.repos.Accounts.UpdateAccountStatus(ctx(), acc.ID, domain.Status429, nil, nil, nil))
	a2, err := tr.repos.Accounts.GetAccount(ctx(), acc.ID)
	require.NoError(t, err)
	require.Equal(t, domain.Status429, a2.Status, "status persisted")
	tr.expectDone(t)
}

// TestGetXxxMissing 单资源 Get 缺 id：空结果集走真实 ent Only 路径 →
// *NotFoundError → errMissingID 映射为 repository.ErrNotFound（消息含缺失 id）。
// 注意刻意用空行（而非 WillReturnError）：生产驱动对无命中返回空集而非错误，
// 只有空集才能触发 ent 的 NotFoundError（驱动错误应原样透传、不伪装 404）。
func TestGetTemplateMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectQuery(q(`FROM "templates" WHERE`)).
		WithArgs(int64(999)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	_, err := tr.repos.Templates.GetTemplate(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

func TestGetAccountMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(999)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	_, err := tr.repos.Accounts.GetAccount(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

func TestGetGroupMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectQuery(q(`FROM "groups" WHERE`)).
		WithArgs(int64(999)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	_, err := tr.repos.Groups.GetGroup(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

// TestDeleteXxxMissing 单资源 Delete 缺 id：DeleteOneID.Exec 对 0 行删除返回
// *NotFoundError（ent 生成：n==0 → NotFoundError）→ errMissingID 映射为
// repository.ErrNotFound（消息含缺失 id，与批量/Get 路径同格式）。与
// TestGetXxxMissing 同基座（真实 ent client + pgxmock）。
func TestDeleteTemplateMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectExec(q(`DELETE FROM "templates"`)).
		WithArgs(int64(999)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	err := tr.repos.Templates.DeleteTemplate(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

func TestDeleteAccountMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectExec(q(`DELETE FROM "accounts"`)).
		WithArgs(int64(999)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	err := tr.repos.Accounts.DeleteAccount(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

func TestDeleteGroupMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectExec(q(`DELETE FROM "groups"`)).
		WithArgs(int64(999)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	err := tr.repos.Groups.DeleteGroup(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

// listSQL 匹配 List 查询的 SQL 片段（含 ORDER BY/LIMIT/OFFSET 断言）。
func listSQL(order string) string {
	return "(?i)FROM \"templates\".*ORDER BY \"templates\"\\." + order + "( LIMIT \\d+)?( OFFSET \\d+)?"
}

func TestListTemplatesQuery(t *testing.T) {
	// 1) name 模糊：Count 与 List 同条件（PG 下 NameContainsFold → ILIKE）。
	t.Run("filter name", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectQuery(q(`SELECT COUNT("templates"."id") FROM "templates"`)).
			WithArgs("%main%").
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))
		tr.pool.ExpectQuery(listSQL(`"id" DESC LIMIT 20`)).
			WithArgs("%main%").
			WillReturnRows(pgxmock.NewRows([]string{"id", "name", "base_url", "supported_formats", "models",
				"format_models", "model_mapping", "created_at", "updated_at"}).
				AddRow(int64(1), "openai-main", "https://api.openai.com/v1",
					[]byte(`["openai-chat","openai-responses"]`), []byte(`["gpt-4o"]`),
					[]byte(`{"openai-responses":["o3"]}`),
					[]byte(`{"gpt-4o":"gpt-4o-2026-01-01"}`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).
				AddRow(int64(2), "openai-alt", "https://api.openai.com/v1",
					[]byte(`["openai-chat"]`), []byte(`["gpt-4o-mini"]`), []byte(`{}`), []byte(`{}`),
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

		rows, total, err := tr.repos.Templates.ListTemplates(ctx(), repository.ListQuery{
			Name: "main",
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
			WillReturnRows(pgxmock.NewRows([]string{"id", "name", "base_url", "supported_formats", "models",
				"format_models", "model_mapping", "created_at", "updated_at"}).
				AddRow(int64(2), "openai-alt", "https://api.openai.com/v1",
					[]byte(`["openai-chat"]`), []byte(`["gpt-4o-mini"]`), []byte(`{}`), []byte(`{}`),
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).
				AddRow(int64(1), "openai-main", "https://api.openai.com/v1",
					[]byte(`["openai-chat","openai-responses"]`), []byte(`["gpt-4o"]`),
					[]byte(`{"openai-responses":["o3"]}`),
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

	// InsertBatch -> 批量 INSERT ... RETURNING id（sqlgraph 批量创建列按名字母序：
	// above_hit, account_id, cache_creation_tokens, cache_read_tokens, cost, ...,
	// input_tokens, ..., output_tokens, overdraft, price_cache_creation_millis,
	// price_cache_read_millis, price_input_millis, price_output_millis, ...,
	// total_tokens, ttft_ms；billing_tier 未设置不落列保持 NULL；l2 新列未设置
	// → 该行 NULL）
	tr.pool.ExpectQuery(q(`INSERT INTO "usage_logs"`)).
		WithArgs(false, int64(2), int64(2), int64(4), int64(0), pgxmock.AnyArg(), "none",
			usagelog.Format("openai-chat"), int64(1), int64(0), int64(10), "m", int64(0), false,
			int64(5678), int64(1234), int64(1e7), int64(2e7), "r1",
			int(200), int64(3), int64(100), int64(88),
			false, int64(2), int64(3), int64(5), int64(0), pgxmock.AnyArg(), "5xx",
			usagelog.Format("openai-chat"), int64(1), int64(0), int64(20), "m", int64(0), false,
			"r2", int(500), int64(3), int64(0)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))

	// Log Query -> Count + SELECT（TTFT 列按 schema 序在 latency_ms 后；价格四列
	// 各紧邻其 tokens 列；计费四列 cost/billing_tier/above_hit/overdraft 在
	// price_cache_creation_millis 与 created_at 之间）
	tr.pool.ExpectQuery(q(`SELECT COUNT(`)).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))
	tr.pool.ExpectQuery(q(`FROM "usage_logs"`)).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "request_id", "group_id", "account_id", "template_id",
			"model", "mapped_model", "format", "status_code", "error_type", "latency_ms",
			"ttft_ms", "input_tokens", "price_input_millis", "output_tokens", "price_output_millis",
			"total_tokens", "cache_read_tokens", "price_cache_read_millis", "cache_creation_tokens",
			"price_cache_creation_millis", "cost", "billing_tier", "above_hit", "overdraft", "created_at"}).
			AddRow(int64(1), "r1", int64(1), int64(2), int64(3), "m", "", "openai-chat",
				int(200), "none", int64(10), int64(88), int64(0), int64(1e7), int64(0), int64(2e7),
				int64(100), int64(4), int64(1234), int64(2), int64(5678),
				int64(0), "", false, false,
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).
			AddRow(int64(2), "r2", int64(1), int64(2), int64(3), "m", "", "openai-chat",
				int(500), "5xx", int64(20), sql.NullInt64{}, int64(0), sql.NullInt64{}, int64(0), sql.NullInt64{},
				int64(0), int64(5), sql.NullInt64{}, int64(3), sql.NullInt64{},
				int64(0), "", false, false,
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

	// Stats Upsert 已改 COPY 两阶段（#17），需要 pgx 原生连接池（NewWithPG
	// 注入）：pgxmock 的 Acquire 未实现（无法 mock COPY）→ New 未注入池时
	// Upsert 返回显式错误，不静默降级。COPY 语义覆盖在真实 PG
	// （pg_stat_test.go TestPGStatUpsertConflictAccumulates / TestPGStatUpsertCopyBulk）。
	bucket := time.Now().Truncate(time.Hour)
	require.Error(t, tr.repos.Stats.Upsert(ctx(), []*domain.StatBucket{
		{BucketTime: bucket, GroupID: 1, Model: "m", RequestCount: 2, ErrorCount: 1, TotalTokens: 100, TotalLatencyMS: 30, CacheReadTokens: 4, CacheCreationTokens: 2},
	}), "未注入 pgx 池（New）→ 显式错误")

	logs := []*domain.UsageLog{
		{RequestID: "r1", GroupID: 1, AccountID: 2, TemplateID: 3, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, LatencyMS: 10, TotalTokens: 100, CacheReadTokens: 4, CacheCreationTokens: 2,
			TTFTMS:                   int64Ptr(88),
			PriceInputMillis:         int64Ptr(1e7),
			PriceOutputMillis:        int64Ptr(2e7),
			PriceCacheReadMillis:     int64Ptr(1234),
			PriceCacheCreationMillis: int64Ptr(5678)},
		{RequestID: "r2", GroupID: 1, AccountID: 2, TemplateID: 3, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 500, ErrorType: domain.Err5xx, LatencyMS: 20, TotalTokens: 0, CacheReadTokens: 5, CacheCreationTokens: 3},
	}
	require.NoError(t, tr.repos.Logs.InsertBatch(ctx(), logs))
	rows, total, err := tr.repos.Logs.QueryLogs(ctx(), repository.LogQuery{GroupID: 1, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, rows, 2)
	require.Equal(t, int64(4), rows[0].CacheReadTokens, "cache read round-trip")
	require.Equal(t, int64(2), rows[0].CacheCreationTokens, "cache creation round-trip")
	require.Equal(t, int64(5), rows[1].CacheReadTokens)
	require.Equal(t, int64(3), rows[1].CacheCreationTokens)
	require.Equal(t, int64(0), rows[0].Cost, "cost round-trip")
	require.Equal(t, "", rows[0].BillingTier, "billing_tier round-trip（空 = 未计费）")
	require.False(t, rows[0].AboveHit, "above_hit round-trip")
	require.False(t, rows[0].Overdraft, "overdraft round-trip")
	// 时间/价格快照五列 round-trip：l1 有值读回；l2 未设置 → NULL（nil）
	require.Equal(t, int64(88), *rows[0].TTFTMS, "ttft_ms round-trip")
	require.Equal(t, int64(1e7), *rows[0].PriceInputMillis, "price_input_millis round-trip")
	require.Equal(t, int64(2e7), *rows[0].PriceOutputMillis, "price_output_millis round-trip")
	require.Equal(t, int64(1234), *rows[0].PriceCacheReadMillis, "price_cache_read_millis round-trip")
	require.Equal(t, int64(5678), *rows[0].PriceCacheCreationMillis, "price_cache_creation_millis round-trip")
	require.Nil(t, rows[1].TTFTMS, "未设置 ttft_ms → NULL")
	require.Nil(t, rows[1].PriceInputMillis, "未设置 price_input_millis → NULL")
	require.Nil(t, rows[1].PriceOutputMillis, "未设置 price_output_millis → NULL")
	require.Nil(t, rows[1].PriceCacheReadMillis, "未设置 price_cache_read_millis → NULL")
	require.Nil(t, rows[1].PriceCacheCreationMillis, "未设置 price_cache_creation_millis → NULL")
	// Stats Scan（测试未过滤 group_id，仅 bucket_time 范围两个参数）
	tr.pool.ExpectQuery(q(`FROM "usage_stats"`)).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucket_time", "group_id", "account_id", "template_id",
			"model", "is_error", "request_count", "error_count", "input_tokens",
			"output_tokens", "total_tokens", "cache_read_tokens", "cache_creation_tokens",
			"cost", "total_latency_ms", "updated_at"}).
			AddRow(int64(1), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), int64(1), int64(0), int64(0),
				"m", false, int64(5), int64(1), int64(0), int64(0), int64(300), int64(10), int64(5), int64(0), int64(30),
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	scanned, err := tr.repos.Stats.ScanStats(ctx(), repository.StatQuery{From: bucket, To: bucket.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, scanned, 1)
	require.Equal(t, int64(5), scanned[0].RequestCount, "upsert accumulates")
	require.Equal(t, int64(300), scanned[0].TotalTokens)
	require.Equal(t, int64(10), scanned[0].CacheReadTokens, "cache read accumulates")
	require.Equal(t, int64(5), scanned[0].CacheCreationTokens, "cache creation accumulates")
	require.Equal(t, int64(0), scanned[0].Cost, "cost accumulates")
	tr.expectDone(t)
}

// ---------------------------------------------------------------------------
// Task 3: 批量删除/更新（ent.Tx 事务，全成或全败）
// ---------------------------------------------------------------------------

func TestDeleteTemplatesBatchRollback(t *testing.T) {
	// 场景 1：存在性检查缺失 id → ErrNotFound（含缺失 id），且无任何 DELETE 执行。
	t.Run("missing id returns ErrNotFound without DELETE", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectBegin()
		// 存在性检查：SELECT id WHERE id IN (1,2,3) 只返回 2 行（id=3 缺失）
		tr.pool.ExpectQuery(q(`SELECT "templates"."id" FROM "templates" WHERE`)).
			WithArgs(int64(1), int64(2), int64(3)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
		tr.pool.ExpectRollback()

		err := tr.repos.Templates.DeleteTemplatesBatch(ctx(), []int64{1, 2, 3})
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.Contains(t, err.Error(), "id=3 missing")
		tr.expectDone(t)
	})

	// 场景 2：存在性通过 → 逐个 DELETE → 中途 DB 错误 → 整体回滚（无 Commit）。
	t.Run("midway db error rolls back without commit", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectBegin()
		tr.pool.ExpectQuery(q(`SELECT "templates"."id" FROM "templates" WHERE`)).
			WithArgs(int64(1), int64(2)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
		tr.pool.ExpectExec(q(`DELETE FROM "templates"`)).
			WithArgs(int64(1)).
			WillReturnResult(pgxmock.NewResult("DELETE", 1))
		tr.pool.ExpectExec(q(`DELETE FROM "templates"`)).
			WithArgs(int64(2)).
			WillReturnError(errors.New("midway db error"))
		tr.pool.ExpectRollback()

		err := tr.repos.Templates.DeleteTemplatesBatch(ctx(), []int64{1, 2})
		require.Error(t, err)
		require.NotErrorIs(t, err, repository.ErrNotFound, "DB 错误不应伪装成 not found")
		tr.expectDone(t)
	})
}

func TestUpdateAccountsBatch(t *testing.T) {
	tr := newRepos(t)
	name := "renamed-acc"
	weight := 50
	st := domain.StatusActive

	tr.pool.ExpectBegin()
	// 存在性检查：ids 全部存在
	tr.pool.ExpectQuery(q(`SELECT "accounts"."id" FROM "accounts" WHERE`)).
		WithArgs(int64(2), int64(5)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(2)).AddRow(int64(5)))
	// 每个 id：UPDATE 只含 patch 提供的字段（name/status/weight + updated_at），
	// 无 template_id/upstream_key/max_concurrency —— WithArgs 精确断言 Set 链列。
	tr.pool.ExpectExec(q(`UPDATE "accounts" SET`)).
		WithArgs(name, account.Status("active"), weight, pgxmock.AnyArg(), int64(2)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(2)).
		WillReturnRows(accountRow("active"))
	tr.pool.ExpectExec(q(`UPDATE "accounts" SET`)).
		WithArgs(name, account.Status("active"), weight, pgxmock.AnyArg(), int64(5)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(5)).
		WillReturnRows(accountRow("active"))
	tr.pool.ExpectCommit()

	err := tr.repos.Accounts.UpdateAccountsBatch(ctx(), []int64{2, 5}, repository.AccountPatch{
		Name: &name, Status: &st, Weight: &weight,
	})
	require.NoError(t, err)
	tr.expectDone(t)
}
