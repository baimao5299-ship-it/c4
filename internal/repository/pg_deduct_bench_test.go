package repository_test

// 热点修复 A 测量基准（spec 2026-08-11）：DeductAndLog 单事务耗时构成测量与
// 优化前后对比。真实 PG（TEST_DATABASE_URL，独立库）+ pg_stat_statements
// （容器需 -c shared_preload_libraries=pg_stat_statements 启动）：
//   - 每次调用整事务 Go 侧耗时（含网络往返），≥7 轮取中位数（首轮预热）；
//   - 运行窗口内 pg_stat_statements 按语句类型拆分服务器侧耗时与往返次数
//     （FEFO SELECT / 条件 UPDATE / 余额回读 / 每个 CreateBulk 批插入 / COMMIT）。
//
// 用法（默认跳过，显式开启）：
//   HOTFIX_BENCH=1 TEST_DATABASE_URL=postgres://postgres:gpm@localhost:15433/gpm_test_hotfix \
//     go test ./internal/repository/ -run TestDeductBenchComposition -v

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// deductBenchRounds 每场景测量轮数（≥5 轮 + 首轮预热，取中位数）。
const deductBenchRounds = 7

// deductBenchLogsPerTx 每事务日志行数 = maxUsageLogsPerTx（billing/flusher.go）
// 10_000——mix3 单用户风暴单事务满档形态。
const deductBenchLogsPerTx = 10_000

// benchLogFor 生产形态计费日志（fullLogFor 全列：26 列 × 行数 = 参数数，
// 65535 上限的最坏界——比生产 19 列更保守）。
func benchLogFor(userID int64, requestID string) *domain.UsageLog {
	return fullLogFor(userID, requestID)
}

// TestDeductBenchComposition DeductAndLog 单事务耗时构成（含各环节往返次数与
// 耗时）。结果打印为 medians + pg_stat_statements 拆分明细。
func TestDeductBenchComposition(t *testing.T) {
	if os.Getenv("HOTFIX_BENCH") == "" {
		t.Skip("HOTFIX_BENCH not set; skipping benchmark (use: HOTFIX_BENCH=1 ... -run TestDeductBenchComposition)")
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	require.NotEmpty(t, dsn, "TEST_DATABASE_URL must be set (dedicated benchmark PG)")
	repos := newPGRepos(t)
	ctx := context.Background()

	// pg_stat_statements 窗口清零（重置在本测试全部 schema 建立之后——统计面
	// 只含 DeductAndLog 语句）。扩展装在 public schema，newPGRepos 的
	// DROP SCHEMA public CASCADE 会连带删掉扩展对象——此处幂等重建。
	statPool, err := repository.OpenPG(ctx, dsn, 2)
	require.NoError(t, err)
	defer statPool.Close()
	_, err = statPool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`)
	require.NoError(t, err)
	_, err = statPool.Exec(ctx, `SELECT pg_stat_statements_reset()`)
	require.NoError(t, err)

	// 主场景：10k 行/事务（maxUsageLogsPerTx 满档，7 轮取中位数）。
	durations := benchDeductRounds(t, repos, 0, deductBenchLogsPerTx, deductBenchRounds)
	printBenchReport(t, "10k 行/事务", durations)

	// maxUsageLogsPerTx 档位对比（同 2000 行/批）：2k/5k 档单事务固定开销
	// （BEGIN+FEFO+扣费+回读+COMMIT 5 往返）摊薄行数少 → 每行往返成本更高
	// 的验证数据。
	durations2k := benchDeductRounds(t, repos, 0, 2_000, 5)
	printBenchReport(t, "2k 行/事务（档位对比）", durations2k)
	durations5k := benchDeductRounds(t, repos, 0, 5_000, 5)
	printBenchReport(t, "5k 行/事务（档位对比）", durations5k)

	// 临时额度形态（FEFO SELECT 返回 3 行 → 每行 1 次条件 UPDATE 往返）——
	// 测量"有临时额度时 FEFO 环节的往返成本"。
	durationsTB := benchDeductRounds(t, repos, 3, deductBenchLogsPerTx, 3)
	printBenchReport(t, "10k 行/事务 + temp balances (FEFO 3 行)", durationsTB)

	// 客户端构建器构造（无 DB 往返）：单事务 10k 行 builder 链的纯客户端耗时
	// 分解——墙钟 ~240ms − 服务器侧 ~66ms 的差值归属（构建 vs 网络往返）。
	durationsBuild := benchBuildersOnly(t, repos, 5)
	printBenchReport(t, "client: 10k builders 构造（无 DB）", durationsBuild)

	// 网络往返延迟直接测量（"往返次数多"假说的决定性实验）：同一连接上 25 次
	// 顺序 SELECT 1——若 25 往返耗时显著（如 >20ms），往返数才是真瓶颈。
	durationsRT := benchRoundTrips(t, statPool, 25, 5)
	printBenchReport(t, "client: 25× SELECT 1 顺序往返", durationsRT)

	printStatBreakdown(t, statPool, ctx)
}

// benchDeductRounds 跑 n 轮 DeductAndLog（每轮独立用户；tempRows 为预插的
// 临时额度行数；perTx 为每事务日志行数），返回逐轮耗时。
func benchDeductRounds(t *testing.T, repos *repository.Repository, tempRows, perTx, n int) []time.Duration {
	t.Helper()
	ctx := context.Background()
	durations := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		u := seedPGUser(t, repos, fmt.Sprintf("bench-%d-%d-%d@example.com", tempRows, perTx, i))
		require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000_000))
		for k := 0; k < tempRows; k++ {
			seedTempBalance(t, repos, u.ID, 100_000, nil)
		}
		logs := make([]*domain.UsageLog, 0, perTx)
		for j := 0; j < perTx; j++ {
			logs = append(logs, benchLogFor(u.ID, fmt.Sprintf("bench-%d-%d-%d-%d", tempRows, perTx, i, j)))
		}
		start := time.Now()
		od, bal, err := repos.DeductAndLog(ctx, u.ID, int64(perTx)*130, logs)
		d := time.Since(start)
		require.NoError(t, err)
		require.False(t, od)
		require.Greater(t, bal, int64(0))
		durations = append(durations, d)
	}
	return durations
}

// benchRoundTrips 网络往返延迟测量：单连接上 n 次顺序 SELECT 1（每轮
// 25 往返 ≈ 单事务 500 行/批档的往返次数；2000 行/批档为 10 往返）。
func benchRoundTrips(t *testing.T, pool *pgxpool.Pool, n, rounds int) []time.Duration {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	durations := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		for j := 0; j < n; j++ {
			var one int
			require.NoError(t, conn.QueryRow(ctx, `SELECT 1`).Scan(&one))
		}
		durations = append(durations, time.Since(start))
	}
	return durations
}

// benchBuildersOnly 纯客户端测量：构造 10k 行 ent builder（不执行 Save、无 DB
// 往返）——分解墙钟中"客户端构建"与"网络往返/解码"的占比。builder 链镜像
// usage_repo.go buildUsageLogCreate（benchmark 用复制，避免导出内部实现）。
func benchBuildersOnly(t *testing.T, repos *repository.Repository, n int) []time.Duration {
	t.Helper()
	ctx := context.Background()
	durations := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		u := seedPGUser(t, repos, fmt.Sprintf("bench-build-%d@example.com", i))
		require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000_000))
		logs := make([]*domain.UsageLog, 0, deductBenchLogsPerTx)
		for j := 0; j < deductBenchLogsPerTx; j++ {
			logs = append(logs, benchLogFor(u.ID, fmt.Sprintf("bench-build-%d-%d", i, j)))
		}
		start := time.Now()
		for _, l := range logs {
			c := repos.Client.UsageLog.Create().
				SetRequestID(l.RequestID).
				SetModel(l.Model).
				SetFormat("openai-chat").
				SetErrorType(string(l.ErrorType)).
				SetLatencyMs(l.LatencyMS).
				SetInputTokens(l.InputTokens).
				SetOutputTokens(l.OutputTokens).
				SetTotalTokens(l.TotalTokens).
				SetCacheReadTokens(l.CacheReadTokens).
				SetCacheCreationTokens(l.CacheCreationTokens).
				SetCost(l.Cost).
				SetAboveHit(l.AboveHit).
				SetOverdraft(l.Overdraft).
				SetCreatedAt(l.CreatedAt).
				SetGroupID(l.GroupID).
				SetAccountID(l.AccountID).
				SetTemplateID(l.TemplateID).
				SetUserID(l.UserID).
				SetKeyID(l.KeyID).
				SetMappedModel(l.MappedModel).
				SetBillingTier(l.BillingTier)
			if l.TTFTMS != nil {
				c = c.SetTtftMs(*l.TTFTMS)
			}
			if l.PriceInputMillis != nil {
				c = c.SetPriceInputMillis(*l.PriceInputMillis)
			}
			if l.PriceOutputMillis != nil {
				c = c.SetPriceOutputMillis(*l.PriceOutputMillis)
			}
			if l.PriceCacheReadMillis != nil {
				c = c.SetPriceCacheReadMillis(*l.PriceCacheReadMillis)
			}
			if l.PriceCacheCreationMillis != nil {
				c = c.SetPriceCacheCreationMillis(*l.PriceCacheCreationMillis)
			}
			_ = c
		}
		durations = append(durations, time.Since(start))
	}
	return durations
}

// printBenchReport 逐轮耗时 + 中位数（排序后中间值）。
func printBenchReport(t *testing.T, label string, durations []time.Duration) {
	t.Helper()
	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	median := sorted[len(sorted)/2]
	t.Logf("== %s: %d 轮 中位数 %s", label, len(durations), median)
	for i, d := range durations {
		t.Logf("   round %d: %s", i, d)
	}
}

// printStatBreakdown pg_stat_statements 拆分明细（服务器侧耗时 + 往返次数）：
// 窗口内全部语句按类型分类汇总——专用容器，无其他会话干扰。
func printStatBreakdown(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT query, calls, total_exec_time, rows
		FROM pg_stat_statements ORDER BY total_exec_time DESC`)
	require.NoError(t, err)
	defer rows.Close()

	type stat struct {
		calls     int64
		totalTime float64 // ms
		totalRows int64
	}
	byType := map[string]*stat{}
	order := []string{}
	for rows.Next() {
		var query string
		var calls, r int64
		var total float64
		require.NoError(t, rows.Scan(&query, &calls, &total, &r))
		if calls == 0 {
			continue
		}
		typ := classifyBenchQuery(query)
		s, ok := byType[typ]
		if !ok {
			s = &stat{}
			byType[typ] = s
			order = append(order, typ)
		}
		s.calls += calls
		s.totalTime += total
		s.totalRows += r
	}
	require.NoError(t, rows.Err())
	t.Logf("== pg_stat_statements 拆分明细（服务器侧耗时）:")
	for _, typ := range order {
		s := byType[typ]
		mean := s.totalTime / float64(s.calls)
		t.Logf("   %-22s calls=%5d total=%8.2fms mean=%7.3fms rows=%d", typ, s.calls, s.totalTime, mean, s.totalRows)
	}
}

// classifyBenchQuery 按语句特征分类（DeductAndLog 事务构成环节）。
func classifyBenchQuery(query string) string {
	q := strings.ToLower(query)
	switch {
	case strings.Contains(q, "temp_balances"):
		if strings.Contains(q, "update") {
			return "UPDATE temp_balances"
		}
		return "SELECT temp_balances (FEFO)"
	case strings.Contains(q, `"users"`):
		if strings.Contains(q, "update") {
			return "UPDATE users (条件扣费)"
		}
		return "SELECT users (余额回读)"
	case strings.Contains(q, "usage_logs"):
		return "INSERT usage_logs (CreateBulk)"
	case strings.Contains(q, "begin"):
		return "BEGIN"
	case strings.Contains(q, "commit"):
		return "COMMIT"
	case strings.Contains(q, "rollback"):
		return "ROLLBACK"
	default:
		return "other"
	}
}
