// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/logx"
)

type noopLogInserter struct{}

func (noopLogInserter) InsertBatch(ctx context.Context, l []*domain.UsageLog) error { return nil }

type noopStatUpserter struct{}

func (noopStatUpserter) Upsert(ctx context.Context, b []*domain.StatBucket) error { return nil }

// captureStatUpserter 记录 Upsert 批（M-3：billed 日志 Aggregate 进 StatBucket）。
type captureStatUpserter struct {
	mu      sync.Mutex
	buckets []*domain.StatBucket
}

func (s *captureStatUpserter) Upsert(ctx context.Context, b []*domain.StatBucket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets = append(s.buckets, b...)
	return nil
}

// fakeDeductWriter DeductAndLog 记录 + 注入失败（fails[uid] = 剩余失败次数，
// 命中时该次调用失败并递减——回灌后重试成功）。
type fakeDeductWriter struct {
	mu    sync.Mutex
	calls []deductCall
	fails map[int64]int
}

type deductCall struct {
	userID, cost int64
	logs         []*domain.UsageLog
}

func (f *fakeDeductWriter) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.fails[userID]; n > 0 {
		f.fails[userID] = n - 1
		return false, 0, errors.New("injected deduct failure")
	}
	f.calls = append(f.calls, deductCall{userID: userID, cost: cost, logs: logs})
	return false, 900000, nil
}

// blockingWriter DeductAndLog 首调阻塞至 blocked 关闭（模拟慢 DB；started 通知
// flush 已换批、worker 在途）。
type blockingWriter struct {
	mu      sync.Mutex
	blocked chan struct{} // 非 nil = 首调阻塞至此关闭
	started chan struct{} // 首调已开始（已 close）
	calls   []deductCall
}

func (w *blockingWriter) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	w.mu.Lock()
	if w.blocked != nil {
		close(w.started)
		ch := w.blocked
		w.blocked = nil
		w.mu.Unlock()
		<-ch
		w.mu.Lock()
	}
	w.calls = append(w.calls, deductCall{userID: userID, cost: cost, logs: logs})
	w.mu.Unlock()
	return false, 900000, nil
}

// barrierWriter 并发屏障（评审 I-：并行峰值断言不得依赖调度时机）：第 barrier
// 个在途调用到齐后一起放行（此后通道已闭，后续调用直接通过）——4 分片 × 首
// user 必然同时到齐，单核/慢 CI 不 flake（此前 20ms sleep 放大窗口在慢机上有
// 并发不足风险）。
type barrierWriter struct {
	mu       sync.Mutex
	barrier  int
	calls    []deductCall
	cur, max int
	release  chan struct{}
	released bool
}

func newBarrierWriter(barrier int) *barrierWriter {
	return &barrierWriter{barrier: barrier, release: make(chan struct{})}
}

func (w *barrierWriter) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	w.mu.Lock()
	w.cur++
	if w.cur > w.max {
		w.max = w.cur
	}
	if !w.released && w.cur == w.barrier {
		w.released = true
		close(w.release)
	}
	ch := w.release
	w.mu.Unlock()
	<-ch
	w.mu.Lock()
	w.cur--
	w.calls = append(w.calls, deductCall{userID: userID, cost: cost, logs: logs})
	w.mu.Unlock()
	return false, 900000, nil
}

// ctxWriter DeductAndLog 尊重 ctx（模拟可取消的慢 DB）：latency 内 ctx 到期 →
// 返回 ctx.Err（在途事务取消语义），否则记录调用。started 非 nil 时首调进入
// 即关闭（测试等待在途批次开始；Once 防并发/重复 close）。
type ctxWriter struct {
	mu        sync.Mutex
	latency   time.Duration
	startOnce sync.Once
	started   chan struct{}
	calls     []deductCall
}

func (w *ctxWriter) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	if w.started != nil { // started 仅构造期赋值、只读——nil 检查无竞态
		w.startOnce.Do(func() { close(w.started) })
	}
	select {
	case <-ctx.Done():
		return false, 0, ctx.Err()
	case <-time.After(w.latency):
	}
	w.mu.Lock()
	w.calls = append(w.calls, deductCall{userID: userID, cost: cost, logs: logs})
	w.mu.Unlock()
	return false, 900000, nil
}

func newTestFlusher(writer DeductWriter) *Flusher {
	return newTestFlusherWorkers(writer, 1)
}

// newTestFlusherWorkers 同 newTestFlusher，指定并行 worker 数（分片测试）。
// worker 数必须经 FlushConfig 传入构造（直接改 f.workers 会让 failCounts
// 分片槽位与分片数错位——毒 chunk 止损计数越界）。
func newTestFlusherWorkers(writer DeductWriter, workers int) *Flusher {
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, noopLogInserter{}, nil)
	bal := NewBalances(fakeBalLoader{m: map[int64]int64{1: 1000, 2: 1000}}, nil)
	return NewFlusher(FlushConfig{
		FlushInterval:          time.Hour,
		BalanceRefreshInterval: time.Hour,
		Workers:                workers,
	}, writer, rec, bal, nil)
}

// TestFlusherGroupsByUser 聚合分组：按 userID 归并 cost + 日志，Close 排空 +
// 全量 flush 后每用户一笔 DeductAndLog 事务（分片并行下同 user 恒同一笔）。
func TestFlusherGroupsByUser(t *testing.T) {
	writer := &fakeDeductWriter{}
	f := newTestFlusherWorkers(writer, 4)
	f.Record(&domain.UsageLog{UserID: 1, Cost: 100})
	f.Record(&domain.UsageLog{UserID: 2, Cost: 200})
	f.Record(&domain.UsageLog{UserID: 1, Cost: 300})
	require.NoError(t, f.Close(context.Background())) // 未 Start：排空 + 全量 flush

	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 2, "按 user 分组两笔事务")
	byUID := map[int64]deductCall{}
	for _, c := range writer.calls {
		byUID[c.userID] = c
	}
	require.Equal(t, int64(400), byUID[1].cost, "同用户 cost 聚合")
	require.Len(t, byUID[1].logs, 2, "同用户日志聚合")
	require.Equal(t, int64(200), byUID[2].cost)
	require.Len(t, byUID[2].logs, 1)
}

// TestFlusherRefillsOnFailure 失败回灌（评审 C-2）：扣费失败 → cost+logs 一起
// 回灌，Close 重试整体重放（明细不丢）。
func TestFlusherRefillsOnFailure(t *testing.T) {
	writer := &fakeDeductWriter{fails: map[int64]int{1: 1}}
	f := newTestFlusher(writer)
	f.Record(&domain.UsageLog{UserID: 1, Cost: 100})
	f.Record(&domain.UsageLog{UserID: 1, Cost: 50})
	require.NoError(t, f.Close(context.Background()))

	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1, "首调失败不计成功，回灌后重试成功")
	require.Equal(t, int64(150), writer.calls[0].cost, "回灌 cost 不丢")
	require.Len(t, writer.calls[0].logs, 2, "回灌带日志（只回 cost 会丢明细）")
}

// TestFlusherCloseIdempotent Close 幂等：第二次 Close 不再 flush（writer 调用数
// 不变）；未 Start 也安全。
func TestFlusherCloseIdempotent(t *testing.T) {
	writer := &fakeDeductWriter{}
	f := newTestFlusher(writer)
	f.Record(&domain.UsageLog{UserID: 1, Cost: 100})
	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	n := len(writer.calls)
	writer.mu.Unlock()
	require.Equal(t, 1, n)
	require.NoError(t, f.Close(context.Background())) // 幂等 no-op
	writer.mu.Lock()
	require.Equal(t, n, len(writer.calls), "重复 Close 不重复扣费")
	writer.mu.Unlock()
}

// TestFlusherCloseAfterStart 已 Start 的优雅排空：取消 loop ctx → loop 退出前
// flush 一次（ctx.Done 路径）→ Close 等 loopDone + 兜底全量 flush——日志不丢。
func TestFlusherCloseAfterStart(t *testing.T) {
	writer := &fakeDeductWriter{}
	f := newTestFlusher(writer)
	loopCtx, loopCancel := context.WithCancel(context.Background())
	require.NoError(t, f.Start(loopCtx))
	f.Record(&domain.UsageLog{UserID: 1, Cost: 100})
	loopCancel()
	require.NoError(t, f.Close(context.Background()))

	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Equal(t, int64(100), writer.calls[0].cost)
	require.Len(t, writer.calls[0].logs, 1)
}

// TestFlusherRecordNeverBlocks O1 管道化核心：Record 无 channel 永不阻塞——
// 无消费方下大量记录必须全部完成（此前有界 channel cap 16384 饱和阻塞在
// proxy.finish() 内是压测塌陷根因）。
func TestFlusherRecordNeverBlocks(t *testing.T) {
	writer := &fakeDeductWriter{}
	f := newTestFlusher(writer)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100_000; i++ {
			f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record 永不阻塞（无 channel 反压）")
	}
	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 10, "10 万条按事务行数上限拆 10 笔（单事务 1 万行）")
	var rows, cost int64
	for _, c := range writer.calls {
		require.Len(t, c.logs, maxUsageLogsPerTx, "单事务行数有界")
		rows += int64(len(c.logs))
		cost += c.cost
	}
	require.Equal(t, int64(100_000), rows, "全量落库（无丢失）")
	require.Equal(t, int64(100_000), cost, "cost 跨事务总和精确（无重复扣费）")
}

// TestFlusherRecordDuringFlushNotBlocked swap 不阻塞：flush 换批后 Record 继续
// 入新 map（零阻塞），在途批落库后新批由 Close 全量 flush。
func TestFlusherRecordDuringFlushNotBlocked(t *testing.T) {
	writer := &blockingWriter{blocked: make(chan struct{}), started: make(chan struct{})}
	blocked := writer.blocked // 首调消费后置 nil，测试须持本地引用
	f := newTestFlusher(writer)
	f.Record(&domain.UsageLog{UserID: 1, Cost: 100})

	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		f.flush() // 首批：worker 阻塞在途
	}()
	<-writer.started // flush 已换批、DeductAndLog 在途

	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		f.Record(&domain.UsageLog{UserID: 1, Cost: 200}) // flush 期间入新 pending map
	}()
	select {
	case <-recDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("flush 期间 Record 不得阻塞（swap 后入新 map）")
	}
	close(blocked) // 放行首批
	<-flushDone

	require.NoError(t, f.Close(context.Background())) // 兜底全量 flush（二批）
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 2, "两批各自落库")
	require.Equal(t, int64(100), writer.calls[0].cost)
	require.Equal(t, int64(200), writer.calls[1].cost)
}

// TestFlusherFlushSerialized 单 flush 入口串行（评审 I-1）：ticker/ctx.Done/Close
// 三处触发共用 flushMu——在途 flush 未完成时第二个 flush 不得并发换批/落库。
func TestFlusherFlushSerialized(t *testing.T) {
	writer := &blockingWriter{blocked: make(chan struct{}), started: make(chan struct{})}
	blocked := writer.blocked
	f := newTestFlusherWorkers(writer, 4)
	f.Record(&domain.UsageLog{UserID: 1, Cost: 100})

	first := make(chan struct{})
	go func() {
		defer close(first)
		f.flush()
	}()
	<-writer.started

	f.Record(&domain.UsageLog{UserID: 1, Cost: 200}) // 进新 pending map
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		f.flush() // 与在途 flush 竞争 flushMu
	}()
	select {
	case <-secondDone:
		t.Fatal("在途 flush 未完成时第二个 flush 必须等待（单入口串行）")
	case <-time.After(100 * time.Millisecond):
	}
	close(blocked) // 放行首批
	<-first
	<-secondDone

	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 2, "两批按序落库（无并发换批）")
	require.Equal(t, int64(100), writer.calls[0].cost)
	require.Equal(t, int64(200), writer.calls[1].cost)
}

// TestFlusherParallelWorkers 分片并行：N worker 并发逐 user DeductAndLog——
// 峰值并发 = worker 数（8 user / 4 worker，barrier 构造保证到齐，确定性断言），
// 每用户恰好一笔（同 user 恒同桶）。
func TestFlusherParallelWorkers(t *testing.T) {
	writer := newBarrierWriter(4)
	f := newTestFlusherWorkers(writer, 4)
	for i := 0; i < 8; i++ {
		f.Record(&domain.UsageLog{UserID: int64(i + 1), Cost: 10})
	}
	require.NoError(t, f.Close(context.Background()))

	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 8, "每用户一笔事务")
	byUID := map[int64]deductCall{}
	for _, c := range writer.calls {
		byUID[c.userID] = c
	}
	for i := 0; i < 8; i++ {
		require.Equal(t, int64(10), byUID[int64(i+1)].cost, "user %d 独立聚合", i+1)
	}
	require.Equal(t, 4, writer.max, "4 worker 并行（分片）")
}

// TestFlusherBilledAddsQuota 评审 M-3 更新（spec 2026-08-14 评审 P1-C）：billed
// 日志经 stats.AddQuota 并入 Recorder 同一 quotaUsed map——统计聚合已删除（
// billed 行落库 usage_logs 后由离线聚合 worker 重建），额度两路闭环（billed/
// 非 billed）在此钉死。
func TestFlusherBilledAddsQuota(t *testing.T) {
	writer := &fakeDeductWriter{}
	q := &captureQuotaWriter{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, noopLogInserter{}, nil)
	rec.SetQuotaWriter(q)
	bal := NewBalances(fakeBalLoader{m: map[int64]int64{1: 1000}}, nil)
	f := NewFlusher(FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)

	f.Record(&domain.UsageLog{
		UserID: 1, GroupID: 10, Model: "gpt-4o", KeyID: 42,
		InputTokens: 3, OutputTokens: 5, TotalTokens: 8, Cost: 130,
		CreatedAt: time.Now(),
	})
	require.NoError(t, f.Close(context.Background()))
	require.NoError(t, rec.Close(context.Background()), "Recorder 手动 flush 额度面（未 Start）")

	q.mu.Lock()
	defer q.mu.Unlock()
	require.Equal(t, 1, q.n, "billed 行额度一次批量回写")
	require.Contains(t, q.calls, int64(42008), "billed 行 TotalTokens 并入 quotaUsed（评审 P1-C 闭环）")
}

// captureQuotaWriter 记录 AddQuotaUsed 调用（P1-C billed 额度闭环断言）。
type captureQuotaWriter struct {
	mu    sync.Mutex
	n     int
	calls []int64 // 编码 = key*1000+delta
}

func (q *captureQuotaWriter) AddQuotaUsed(ctx context.Context, deltas map[int64]int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.n++
	for k, d := range deltas {
		q.calls = append(q.calls, k*1000+d)
	}
	return nil
}

// newTestLogger warn 级文件 logger（Warn 断言用；Windows 上 zap 句柄不释放，
// 目录清理 best-effort）。
func newTestLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "flusher-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New("warn", out)
	require.NoError(t, err)
	return logger, out
}

// TestFlusherCloseTruncatesOnBudget 停机排空受 ctx 预算约束（O1 复测根因）：
// deadline 到期 → 截断退出 + Warn（含已排空/剩余条数），不无界阻塞停机；在途
// 事务经 ctx 取消快速失败回灌（不丢，可统计）。无 deadline 完整排空由
// TestFlusherGroupsByUser / TestFlusherCloseAfterStart 等覆盖。
func TestFlusherCloseTruncatesOnBudget(t *testing.T) {
	writer := &ctxWriter{latency: 500 * time.Millisecond}
	f := newTestFlusher(writer)
	f.Record(&domain.UsageLog{UserID: 1, Cost: 100})
	f.Record(&domain.UsageLog{UserID: 2, Cost: 100})
	logger, out := newTestLogger(t)
	f.log = logger

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(150*time.Millisecond))
	defer cancel()
	require.NoError(t, f.Close(ctx))

	require.Equal(t, 2, f.pendingCount(), "预算到期未处理条目回灌不丢")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "shutdown budget exceeded, truncated drain")
	require.Contains(t, string(b), `"flushed_logs":0`)
	require.Contains(t, string(b), `"remaining_logs":2`)
}

// TestFlusherCloseWaitsInflight O2 停机修复核心（复测根因 1）：ticker 批次已
// 在途（baseCtx、pending 已 swap、flushMu 被占）时 Close 必须先等其结束——
// 否则 drain 循环见 pendingCount()==0 静默提前返回，在途批次无界运行：
//   - 预算内完成：Close 实际等待（不提前返回），完整排空，无截断 Warn；
//   - 预算到期：Cancel baseCtx → 在途 DeductAndLog 快速失败（未落库、回灌不
//     丢）→ 截断 Warn（flushed/remaining 条数）+ 快速退出（不等其自然完成）。
func TestFlusherCloseWaitsInflight(t *testing.T) {
	t.Run("waits within budget", func(t *testing.T) {
		writer := &ctxWriter{latency: 500 * time.Millisecond, started: make(chan struct{})}
		f := newTestFlusher(writer)
		f.Record(&domain.UsageLog{UserID: 1, Cost: 100})
		f.Record(&domain.UsageLog{UserID: 2, Cost: 100})
		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			f.flush() // ticker 路径批次（baseCtx）在途
		}()
		<-writer.started

		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2*time.Second))
		defer cancel()
		start := time.Now()
		closeDone := make(chan struct{})
		var closeErr error
		go func() {
			closeErr = f.Close(ctx)
			close(closeDone)
		}()
		select {
		case <-closeDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Close 未在预算内返回（在途批次未被等待）")
		}
		require.NoError(t, closeErr)
		elapsed := time.Since(start)
		require.GreaterOrEqual(t, elapsed, 400*time.Millisecond, "Close 必须等待在途批次完成（不得静默提前返回）")
		require.Less(t, elapsed, 1500*time.Millisecond, "在途批次自然完成后即返回（不得等满预算）")
		writer.mu.Lock()
		require.Len(t, writer.calls, 2, "在途批次完整落库（无截断）")
		writer.mu.Unlock()
		require.Equal(t, 0, f.pendingCount(), "完整排空")
		<-flushDone
	})

	t.Run("cancels on budget expiry", func(t *testing.T) {
		writer := &ctxWriter{latency: 500 * time.Millisecond, started: make(chan struct{})}
		f := newTestFlusher(writer)
		f.Record(&domain.UsageLog{UserID: 1, Cost: 100})
		f.Record(&domain.UsageLog{UserID: 2, Cost: 100})
		logger, out := newTestLogger(t)
		f.log = logger
		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			f.flush()
		}()
		<-writer.started

		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(120*time.Millisecond))
		defer cancel()
		start := time.Now()
		closeDone := make(chan struct{})
		var closeErr error
		go func() {
			closeErr = f.Close(ctx)
			close(closeDone)
		}()
		select {
		case <-closeDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Close 未在预算内返回（在途批次取消未生效）")
		}
		require.NoError(t, closeErr)
		require.Less(t, time.Since(start), 500*time.Millisecond, "在途批次必须被取消快速失败（不得等其自然完成）")
		writer.mu.Lock()
		require.Empty(t, writer.calls, "在途 DeductAndLog 被取消——不得落库成功")
		writer.mu.Unlock()
		require.Equal(t, 2, f.pendingCount(), "取消后回灌不丢")
		require.NoError(t, logger.Sync())
		b, err := os.ReadFile(out)
		require.NoError(t, err)
		require.Contains(t, string(b), "shutdown budget exceeded, truncated drain")
		require.Contains(t, string(b), `"flushed_logs":0`)
		require.Contains(t, string(b), `"remaining_logs":2`)
		<-flushDone
	})
}

// ignoreCtxWriter DeductAndLog 忽略 ctx 永久阻塞（模拟 DB 病态卡死——
// database/sql 取消路径本身被拖住的极端形态；A-P2-8-2 第二 select 兜底目标）。
// 测试结束即弃置（在途 goroutine 无放行通道，属刻意泄漏）。
type ignoreCtxWriter struct {
	started chan struct{} // 首调已进入（测试等待在途批次）
}

func (w *ignoreCtxWriter) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	select {
	case w.started <- struct{}{}:
	default:
	}
	<-make(chan struct{}) // 永久阻塞（不响应 ctx 取消；无发送者，永不返回）
	return false, 0, nil
}

// TestFlusherCloseAbandonsInflightOnTimeout A-P2-8-2（与 usage 包同款）：`<-acquired`
// 第二 select 预算超时——驱动不尊重 ctx 时 Close 不再无界等待：预算到期 → Cancel
// baseCtx → 收尾宽限超时 → Warn 放弃排空、截断退出（在途批次由已取消 baseCtx
// 收尾回灌不丢；后续排空循环都会被 flushMu 挡住，不再触碰）。旧实现无界等待，
// 编排层强杀 → 全量内存 pending 丢失。
func TestFlusherCloseAbandonsInflightOnTimeout(t *testing.T) {
	logger, out := newTestLogger(t)
	old := inflightAbandonGrace
	inflightAbandonGrace = 50 * time.Millisecond
	t.Cleanup(func() { inflightAbandonGrace = old })

	writer := &ignoreCtxWriter{started: make(chan struct{})}
	f := newTestFlusher(writer)
	f.log = logger
	f.Record(&domain.UsageLog{UserID: 1, Cost: 100})

	go f.flush() // ticker 路径批次在途（永久阻塞，不响应取消）
	<-writer.started

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(100*time.Millisecond))
	defer cancel()
	start := time.Now()
	require.NoError(t, f.Close(ctx))
	require.Less(t, time.Since(start), 500*time.Millisecond, "放弃排空快速退出（不得无界等待在途批次）")

	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "billing flusher close: in-flight flush not finished in time, abandoning drain")
	require.Contains(t, string(b), `"level":"warn"`)
}

// TestFlusherWaterlineWarns 水线按聚合日志条数计（评审 C-1）：pending 日志条数
// 超阈值 → Warn（429 风暴 24.5k 日志/s 才是无界增长场景；按去重用户数计 ≤1M
// 用户不可达恒不告警）。注入小阈值触发；flush 回落复位后再超阈值再次 Warn。
func TestFlusherWaterlineWarns(t *testing.T) {
	old := pendingWaterline
	pendingWaterline = 100
	t.Cleanup(func() { pendingWaterline = old })

	logger, out := newTestLogger(t)
	writer := &fakeDeductWriter{}
	f := newTestFlusher(writer)
	f.log = logger
	for i := 0; i < 110; i++ {
		f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
	}
	for f.pendingCount() > 0 {
		f.flush() // 逐事务推进至排空 → 回落复位（pendingN < 水线 → warned 复位）
	}
	for i := 0; i < 110; i++ {
		f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
	}
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(b), "billing pending exceeds waterline"),
		"超阈值 Warn，回落复位后再次超阈值再次 Warn")
	require.Contains(t, string(b), `"waterline":100`)
	require.NoError(t, f.Close(context.Background()))
}

// TestFlusherHugeBacklogDrainsInOneFlush P2a（压测 2026-08-11 复测修复）：
// 单用户巨批不再"每 flush 至多一块"续传（该上限把单用户 drain 钉死在
// 10k 行/s，持续超限到达 → pending 无界增长）——续传循环单次 flush 内逐块
// （≤ maxUsageLogsPerTx）提交至排空：10w+ 积压一次 flush 全量落库（快 writer
// 预算不触发），单事务行数上限不变（CreateBulk 内存有界），cost 拆分跨事务
// 总和精确。
func TestFlusherHugeBacklogDrainsInOneFlush(t *testing.T) {
	const total = 120_000
	writer := &fakeDeductWriter{}
	f := newTestFlusher(writer)
	for i := 0; i < total; i++ {
		f.Record(&domain.UsageLog{UserID: 1, Cost: 3})
	}

	f.flush() // 一次 flush：续传循环逐块提交至排空
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, total/maxUsageLogsPerTx, "单次 flush 全量落库（12 块，不再 10k/次 钉死）")
	var rows, cost int64
	for _, c := range writer.calls {
		require.LessOrEqual(t, len(c.logs), maxUsageLogsPerTx, "单事务行数有界（巨批 CreateBulk 内存不暴涨）")
		rows += int64(len(c.logs))
		cost += c.cost
	}
	require.Equal(t, int64(total), rows, "全量落库（无丢失）")
	require.Equal(t, int64(total*3), cost, "cost 跨事务总和精确（无重复扣费）")
	require.Zero(t, f.pendingN.Load(), "排空后 pending 计数归零")
	require.Zero(t, f.pendingCount(), "排空后 map 空（无续传残留）")
}

// TestFlusherHugeBacklogDoesNotStarveOthers P2：巨批用户拆事务后同批其他用户
// 不被饿死——一次 flush 触发内巨批用户全量续传（多事务）+ 小用户并行落库
// （此前巨批单事务串行 8 分钟，flushMu 冻结全局记录；续传循环的逐事务提交 +
// 分片并行保持同批不冻结）。
func TestFlusherHugeBacklogDoesNotStarveOthers(t *testing.T) {
	writer := &fakeDeductWriter{}
	f := newTestFlusherWorkers(writer, 4)
	for i := 0; i < 100_000; i++ {
		f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
	}
	f.Record(&domain.UsageLog{UserID: 2, Cost: 7})

	f.flush() // 一触发：user1 巨批续传循环全量 + user2 同批并行
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 11, "user1 10 事务 + user2 1 事务同批处理")
	var user1Rows int64
	var user2 *deductCall
	for i := range writer.calls {
		c := writer.calls[i]
		if c.userID == 1 {
			user1Rows += int64(len(c.logs))
			require.LessOrEqual(t, len(c.logs), maxUsageLogsPerTx, "user1 单事务行数上限")
		} else {
			user2 = &writer.calls[i]
		}
	}
	require.NotNil(t, user2, "user2 必须同批处理")
	require.Len(t, user2.logs, 1, "user2 不受巨批用户阻塞")
	require.Equal(t, int64(7), user2.cost)
	require.Equal(t, int64(100_000), user1Rows, "user1 全量落库（续传循环不再 10k/次 钉死）")
	require.Zero(t, f.pendingCount(), "无续传残留")
	require.Zero(t, f.pendingN.Load())
}

// TestFlusherHugeBacklogChunkFailureRefills P2：拆事务失败回灌语义——失败仅
// 回灌未提交块（每事务原子，部分成功可接受），已提交块不重放（不重复扣费），
// 续传后全量落库。
func TestFlusherHugeBacklogChunkFailureRefills(t *testing.T) {
	writer := &fakeDeductWriter{fails: map[int64]int{1: 1}}
	f := newTestFlusher(writer)
	for i := 0; i < 20_000; i++ {
		f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
	}

	f.flush() // 首个事务失败 → chunk+rest 整体回灌
	require.Equal(t, 1, f.pendingCount())
	require.Equal(t, int64(20_000), f.pendingN.Load(), "失败事务整体回灌（明细不丢）")

	for f.pendingCount() > 0 {
		f.flush()
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 2, "首事务失败不计成功；重试后 2 个事务提交")
	var rows, cost int64
	for _, c := range writer.calls {
		require.LessOrEqual(t, len(c.logs), maxUsageLogsPerTx)
		rows += int64(len(c.logs))
		cost += c.cost
	}
	require.Equal(t, int64(20_000), rows, "全量落库（无重复无丢失）")
	require.Equal(t, int64(20_000), cost, "cost 精确")
	require.Zero(t, f.pendingN.Load())
}

// TestFlusherStormArrivalDrainedInSameFlush P2a：风暴到达有界——flush 在途时
// 持续 Record（单 key 高压/429 风暴形态），续传循环在本轮 flush 内一并落库：
// pending 峰值 = 单 flush 窗口到达量（不跨周期累积），flush 返回后归零。
// 旧实现（每 flush 每用户至多一块）：在途期间到达的 30k 全部留到后续 flush
// 逐个周期消化，pending 以 (到达-10k)/周期 无界增长。
func TestFlusherStormArrivalDrainedInSameFlush(t *testing.T) {
	writer := &blockingWriter{blocked: make(chan struct{}), started: make(chan struct{})}
	blocked := writer.blocked // 首调消费后置 nil，测试须持本地引用
	f := newTestFlusher(writer)
	for i := 0; i < 20_000; i++ { // 初始积压（风暴开始前已存在）
		f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
	}

	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		f.flush() // 首批 worker 阻塞在途
	}()
	<-writer.started // flush 已换批、首个 DeductAndLog 在途

	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		for i := 0; i < 30_000; i++ { // 风暴：flush 在途期间持续到达
			f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
		}
	}()
	<-recDone

	close(blocked) // 放行首批
	<-flushDone
	writer.mu.Lock()
	var rows int64
	for _, c := range writer.calls {
		rows += int64(len(c.logs))
	}
	writer.mu.Unlock()
	require.Equal(t, int64(50_000), rows, "初始积压 + 风暴到达同轮 flush 全量落库")
	require.Zero(t, f.pendingN.Load(), "风暴后 pending 归零（不跨周期累积）")
	require.Zero(t, f.pendingCount(), "风暴后 map 空")
}

// TestFlusherBacklogDrainBudgetBoundsFlush P2a 公平性：续传循环受
// backlogDrainBudget 约束——慢 DB 下每次 flush 至多预算内块数（flushMu 持有
// 时间有界，其他用户 flush 周期不被长期饿死），剩余 refill 下轮续传，逐轮
// 推进最终全量落库。
func TestFlusherBacklogDrainBudgetBoundsFlush(t *testing.T) {
	old := backlogDrainBudget
	backlogDrainBudget = 10 * time.Millisecond
	t.Cleanup(func() { backlogDrainBudget = old })

	// 慢 writer：单事务 30ms > 预算 → 每轮 flush 恰好一块（预算在块间检查，
	// 首块不受门槛约束）
	writer := &ctxWriter{latency: 30 * time.Millisecond}
	f := newTestFlusher(writer)
	const total = 30_000 // 3 块
	for i := 0; i < total; i++ {
		f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
	}

	f.flush() // 第一轮：1 块，其余 refill
	writer.mu.Lock()
	n := len(writer.calls)
	writer.mu.Unlock()
	require.Equal(t, 1, n, "预算内每轮至多一块（慢 DB 下 flushMu 持有有界）")
	require.Equal(t, 1, f.pendingCount(), "剩余回灌下轮续传")
	require.Equal(t, int64(total-maxUsageLogsPerTx), f.pendingN.Load(), "pending 计数同步剩余")

	rounds := 0
	for f.pendingCount() > 0 { // 逐轮推进至排空（每轮一块）
		require.Less(t, rounds, total/maxUsageLogsPerTx+2, "推进轮数有界")
		f.flush()
		rounds++
	}
	require.Equal(t, total/maxUsageLogsPerTx-1, rounds, "剩余 2 块逐轮提交")
	require.Zero(t, f.pendingN.Load())
}

// TestFlusherCloseDrainsHugeBacklog P2b（压测 2026-08-11 复测修复）：停机排空
// 函数级——pending 非空时 Close 在退出前同步 flush 全量（预算内完整排空、无
// 截断 Warn）；续传循环使单用户巨批一次 flushCtx 全量落库（不再 10k/次 钉死
// 拉长排空时间，停机丢量规模收敛到"预算内尽量 flush"的截断路径，后者由
// TestFlusherCloseTruncatesOnBudget 覆盖）。
func TestFlusherCloseDrainsHugeBacklog(t *testing.T) {
	const total = 250_000 // 25 事务
	writer := &fakeDeductWriter{}
	f := newTestFlusher(writer)
	for i := 0; i < total; i++ {
		f.Record(&domain.UsageLog{UserID: 1, Cost: 2})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.Close(ctx)) // 停机：退出前同步 drain pending

	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, total/maxUsageLogsPerTx, "退出前全量落库（每事务行数上限不变）")
	var rows, cost int64
	for _, c := range writer.calls {
		require.LessOrEqual(t, len(c.logs), maxUsageLogsPerTx)
		rows += int64(len(c.logs))
		cost += c.cost
	}
	require.Equal(t, int64(total), rows, "停机不丢（预算内完整排空）")
	require.Equal(t, int64(total*2), cost, "cost 精确（无重复扣费）")
	require.Zero(t, f.pendingN.Load(), "退出前 pending 清空")
}

// TestFlusherChunkCostSumsLogs 方向 A 批次 1c（A-P2-1）：拆块处 chunk.cost 逐条
// 累加明细求和（替代比例公式 e.cost * max / len——比例公式与明细求和脱钩，整数
// 截断可致 chunk.cost=0/成本错位）。非均匀 fixture 双向：前 10k cost=0 后 10k
// cost=100（首块 Σ=0）与前 10k cost=100 后 10k cost=0（首块 Σ>0）——断言每
// 事务 chunk.cost == 明细求和 + 跨事务总和保和（无资金损失）。
func TestFlusherChunkCostSumsLogs(t *testing.T) {
	for _, tc := range []struct {
		name                string
		first, last         int64 // 前 10k / 后 10k 每条 cost
		wantChunk1, wantSum int64 // 首块 cost（== Σ 前 10k）、总 cost
	}{
		{"zero-cost head", 0, 100, 0, 1_000_000},
		{"cost-bearing head", 100, 0, 1_000_000, 1_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &fakeDeductWriter{}
			f := newTestFlusher(writer)
			for i := 0; i < maxUsageLogsPerTx; i++ {
				f.Record(&domain.UsageLog{UserID: 1, Cost: tc.first})
			}
			for i := 0; i < maxUsageLogsPerTx; i++ {
				f.Record(&domain.UsageLog{UserID: 1, Cost: tc.last})
			}

			f.flush() // 20k 行拆 2 块（每块 ≤ maxUsageLogsPerTx 行，单事务有界）

			writer.mu.Lock()
			defer writer.mu.Unlock()
			require.Len(t, writer.calls, 2, "20k 行拆 2 事务")
			require.Equal(t, tc.wantChunk1, writer.calls[0].cost, "首块 cost == 明细求和")
			require.Equal(t, tc.wantSum-tc.wantChunk1, writer.calls[1].cost, "次块 cost == 明细求和（rest 保和）")
			var total int64
			for _, c := range writer.calls {
				total += c.cost
			}
			require.Equal(t, tc.wantSum, total, "跨事务总和保和不变（无资金损失）")
			require.Zero(t, f.pendingN.Load(), "排空无残留")
		})
	}
}

// poisonWriter 注入指定 request_id 的毒日志：含毒行的块恒失败（模拟单块永久
// 失败——分区缺失/DB 长故障的毒 chunk 形态），其余块成功。
type poisonWriter struct {
	mu      sync.Mutex
	poison  string
	poisonN int // 毒块被拒绝的次数（断言弃置前恰 5 次失败往返）
	calls   []deductCall
}

func (w *poisonWriter) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range logs {
		if l.RequestID == w.poison {
			w.poisonN++
			return false, 0, errors.New("injected poison failure")
		}
	}
	w.calls = append(w.calls, deductCall{userID: userID, cost: cost, logs: logs})
	return false, 900000, nil
}

// TestFlusherPoisonChunkIsolated 方向 A 批次 1b（A-P2-2）：毒 chunk 止损——含毒
// 行的块连续失败 ≥ maxLogFlushFailures 次 → Error（含 chunk 首行 request_id）+
// 弃置该块（不 refill）+ 其后剩余下轮继续流动。旧实现：毒块永续回灌，该用户
// 新日志永远排后（免费蹭用无界 + 快照陈旧）。
func TestFlusherPoisonChunkIsolated(t *testing.T) {
	writer := &poisonWriter{poison: "poison-0"}
	f := newTestFlusher(writer)
	logger, out := newTestLogger(t)
	f.log = logger

	const total = 2 * maxUsageLogsPerTx // 20k：毒块（前 10k）+ 正常剩余（后 10k）
	for i := 0; i < total; i++ {
		req := fmt.Sprintf("req-%d", i)
		if i == 0 {
			req = "poison-0"
		}
		f.Record(&domain.UsageLog{UserID: 1, Cost: 1, RequestID: req})
	}

	// 前 4 次失败（未达阈值）：毒块回灌队首重试（失败即停止本 shard——计数
	// 不被其余块打断），块不丢
	for i := 0; i < maxLogFlushFailures-1; i++ {
		f.flush()
		writer.mu.Lock()
		poisonN := writer.poisonN
		writer.mu.Unlock()
		require.Equal(t, i+1, poisonN, "第 %d 轮毒块被拒", i+1)
		require.Equal(t, 1, f.pendingCount(), "毒块回灌待重试")
		require.Equal(t, int64(total), f.pendingN.Load(), "失败块 + 剩余整体回灌（不丢）")
		writer.mu.Lock()
		n := len(writer.calls)
		writer.mu.Unlock()
		require.Zero(t, n, "毒块弃置前无任何成功调用（失败即停止 shard，剩余块不先落库）")
	}

	// 第 5 次失败 = 达阈值：弃置毒块（Error 日志含 request_id），剩余回灌
	f.flush()
	writer.mu.Lock()
	n := len(writer.calls)
	poisonN := writer.poisonN
	writer.mu.Unlock()
	require.Zero(t, n, "毒块弃置前无任何成功调用")
	require.Equal(t, maxLogFlushFailures, poisonN, "恰 5 次失败往返后弃置")
	require.Equal(t, 1, f.pendingCount())
	require.Equal(t, int64(total-maxUsageLogsPerTx), f.pendingN.Load(), "毒块弃置（写销），其后剩余回灌不丢")

	// 后续日志继续流动：剩余 10k 下轮 flush 成功落库
	f.flush()
	writer.mu.Lock()
	require.Len(t, writer.calls, 1, "剩余块成功落库")
	require.Len(t, writer.calls[0].logs, maxUsageLogsPerTx)
	require.Equal(t, int64(maxUsageLogsPerTx), writer.calls[0].cost)
	writer.mu.Unlock()
	require.Zero(t, f.pendingCount(), "毒块弃置 + 剩余排空")

	// 弃置可观测（不静默丢）：Error 日志含首行 request_id + 弃置行数
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "billing deduct failed, dropping poison chunk")
	require.Contains(t, string(b), `"level":"error"`, "止损升级 Error 级（可观测）")
	require.Contains(t, string(b), `"request_id":"poison-0"`)
	require.Contains(t, string(b), `"dropped_logs":10000`)
}

// TestFlusherPoisonStopLossResetsOnSuccess 毒 chunk 止损计数复位（对齐 usage.go
// TestLogPoisonStopLossResetsOnSuccess）：失败后成功推进 → 连续失败计数清零——
// "连续失败 ≥N 次"语义，间隔成功不累计（DB 短时故障不误丢）。
func TestFlusherPoisonStopLossResetsOnSuccess(t *testing.T) {
	writer := &fakeDeductWriter{fails: map[int64]int{1: 4}}
	f := newTestFlusher(writer)
	for i := 0; i < maxUsageLogsPerTx; i++ {
		f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
	}

	// 4 次连续失败（未达阈值）：回灌重试
	for i := 0; i < 4; i++ {
		f.flush()
		require.Equal(t, 1, f.pendingCount())
		require.Equal(t, int64(maxUsageLogsPerTx), f.pendingN.Load())
	}
	require.Equal(t, 4, f.failCounts[0], "4 次连续失败计数")

	// 成功推进 → 计数复位（间隔成功打断连续性）
	f.flush()
	require.Zero(t, f.pendingCount(), "成功落库排空")
	require.Zero(t, f.failCounts[0], "成功推进复位")

	// 重新注入 5 次失败：若复位失效（计数残留 4），首轮失败即达阈值弃置；
	// 复位后须再连续 5 次失败才弃置——断言 5 次注入全被消费即证明从 0 重新累计
	writer.mu.Lock()
	writer.fails[1] = 5
	writer.mu.Unlock()
	f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
	for i := 0; i < 5; i++ {
		f.flush()
	}
	writer.mu.Lock()
	remain := writer.fails[1]
	writer.mu.Unlock()
	require.Zero(t, remain, "5 次注入失败全部消费（复位后从 0 累计，未提前弃置）")
	require.Zero(t, f.pendingCount(), "5 次连续失败后毒块弃置")
}

// conflictWriter DeductAndLog 注入 usage_logs 唯一键冲突（方向 A 批次 1a 重试
// 路径）：pgErr=true → pgx 形态 *pgconn.PgError Code=23505 并经 fmt.Errorf
// 包装（验证 errors.As 解包）；pgErr=false → ent 形态（非 pgconn，错误链全文本
// 含 "violates unique constraint"——ent 生成代码的 ConstraintError 包装形态）。
// failN > 0：前 failN 次调用冲突后成功（COMMIT 歧义窗口形态——先前事务已提交）；
// persist：每次调用都冲突（重试路径每轮撞 23505 的永久毒丸面）。
type conflictWriter struct {
	mu      sync.Mutex
	pgErr   bool
	persist bool
	failN   int
	calls   []deductCall
}

func (w *conflictWriter) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	conflict := w.persist
	if !conflict && w.failN > 0 {
		conflict = true
		w.failN--
	}
	if conflict {
		if w.pgErr {
			return false, 0, fmt.Errorf("deduct failed: %w", &pgconn.PgError{
				Code: "23505", Message: `duplicate key value violates unique constraint "usagelog_request_id_created_at"`,
			})
		}
		return false, 0, fmt.Errorf(`ent: constraint failed: duplicate key value violates unique constraint "usagelog_request_id_created_at" (SQLSTATE 23505)`)
	}
	w.calls = append(w.calls, deductCall{userID: userID, cost: cost, logs: logs})
	return false, 900000, nil
}

// TestFlusherUniqueConflictTreatedAsSuccess 方向 A 批次 1a（A-P2-3）：重试路径
// 撞唯一键冲突 → 按成功处理——不 refill（无重试 = 无双扣路径）、failCounts 不增
// （冲突 ≠ 失败，不得计入毒丸止损）、续传循环继续后续块。两路径均验（pgx
// 23505 经 fmt 包装 + ent 文本形态）。
func TestFlusherUniqueConflictTreatedAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		writer  *conflictWriter
		persist bool
	}{
		{"pgx wrapped once", &conflictWriter{pgErr: true, failN: 1}, false},
		{"ent text once", &conflictWriter{pgErr: false, failN: 1}, false},
		{"pgx persistent", &conflictWriter{pgErr: true, persist: true}, true},
		{"ent text persistent", &conflictWriter{pgErr: false, persist: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := tc.writer
			f := newTestFlusher(writer)
			for i := 0; i < 2*maxUsageLogsPerTx; i++ {
				f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
			}

			f.flush() // 拆 2 块；首块撞冲突（once 形态下次块成功落库）

			writer.mu.Lock()
			defer writer.mu.Unlock()
			if tc.persist {
				require.Empty(t, writer.calls, "恒冲突：全部块按成功处理（先前事务已提交，不重试）")
			} else {
				require.Len(t, writer.calls, 1, "冲突块按成功不计调用（不重试）；次块成功落库")
				require.Len(t, writer.calls[0].logs, maxUsageLogsPerTx, "次块明细完整")
				require.Equal(t, int64(maxUsageLogsPerTx), writer.calls[0].cost)
			}
			require.Zero(t, f.failCounts[0], "冲突不累计 failCounts（防毒丸弃置误杀已提交块）")
			require.Zero(t, f.pendingCount(), "冲突块不 refill（无双扣路径）")
			require.Zero(t, f.pendingN.Load())
		})
	}
}

// TestFlusherLastFlushNotAdvancingOnFailure G2-4（spec 2026-08-13）：lastFlush
// 语义"成功落库时刻"——全失败 flush 不推进（旧实现无条件 Store，失败也推进，
// 监控误判落库健康）；成功落库（含部分成功）才推进。
func TestFlusherLastFlushNotAdvancingOnFailure(t *testing.T) {
	t.Run("all fail does not advance", func(t *testing.T) {
		writer := &fakeDeductWriter{fails: map[int64]int{1: 4}}
		f := newTestFlusher(writer)
		f.Record(&domain.UsageLog{UserID: 1, Cost: 100})

		// 4 轮全失败（< maxLogFlushFailures 止损阈值）：回灌重试，lastFlush 恒 0
		for i := 0; i < 4; i++ {
			f.flush()
			require.Zero(t, f.lastFlush.Load(), "第 %d 轮全失败 flush 不得推进 lastFlush", i+1)
			require.Equal(t, int64(1), f.pendingN.Load(), "失败回灌不丢（日志条数）")
		}
		require.Equal(t, 4, f.failCounts[0], "4 次连续失败计数")

		// 成功落库 → 推进
		f.flush()
		require.Greater(t, f.lastFlush.Load(), int64(0), "成功落库推进 lastFlush")
		require.Zero(t, f.pendingCount(), "成功排空")
	})

	t.Run("partial success advances", func(t *testing.T) {
		// 2 worker 分片：两用户各自独立 shard（同 user 恒同桶），成功/失败并行
		// 确定（1 worker 时 map 迭代序随机——user2 先失败会把 user1 一并回灌）。
		writer := &fakeDeductWriter{fails: map[int64]int{2: 1}}
		f := newTestFlusherWorkers(writer, 2)
		f.Record(&domain.UsageLog{UserID: 1, Cost: 100})
		f.Record(&domain.UsageLog{UserID: 2, Cost: 100})

		f.flush() // user2 失败回灌；user1 成功落库 → 推进（确有日志落库）
		require.Greater(t, f.lastFlush.Load(), int64(0), "部分成功（≥1 chunk 落库）推进 lastFlush")
		require.Equal(t, 1, f.pendingCount(), "user2 失败回灌待重试")

		require.NoError(t, f.Close(context.Background()), "重试排空成功")
		require.Zero(t, f.pendingCount())
	})
}
