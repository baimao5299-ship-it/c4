package billing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/usage"
	"go-proxy-mini/pkg/logx"
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
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		StatsFlushInterval: time.Hour,
	}, noopLogInserter{}, noopStatUpserter{}, nil)
	bal := NewBalances(fakeBalLoader{m: map[int64]int64{1: 1000, 2: 1000}}, nil)
	return NewFlusher(FlushConfig{
		FlushInterval:          time.Hour,
		BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
}

// newTestFlusherWorkers 同 newTestFlusher，指定并行 worker 数（分片测试）。
func newTestFlusherWorkers(writer DeductWriter, workers int) *Flusher {
	f := newTestFlusher(writer)
	f.workers = workers
	return f
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
	require.Len(t, writer.calls, 1, "聚合单笔事务")
	require.Equal(t, int64(100_000), writer.calls[0].cost, "10 万条聚合 cost")
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

// TestFlusherBilledAggregatesStats 评审 M-3：billed 日志经 stats.Aggregate 进
// usagestat 统计面（每日志恰好一个写者——Flusher.Record 即统计写者）。
func TestFlusherBilledAggregatesStats(t *testing.T) {
	writer := &fakeDeductWriter{}
	stats := &captureStatUpserter{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		StatsFlushInterval: time.Hour,
	}, noopLogInserter{}, stats, nil)
	bal := NewBalances(fakeBalLoader{m: map[int64]int64{1: 1000}}, nil)
	f := NewFlusher(FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)

	f.Record(&domain.UsageLog{
		UserID: 1, GroupID: 10, Model: "gpt-4o",
		InputTokens: 3, OutputTokens: 5, Cost: 130,
		CreatedAt: time.Now(),
	})
	require.NoError(t, f.Close(context.Background()))
	require.NoError(t, rec.Close(context.Background()), "Recorder 手动 flush 统计面（未 Start）")

	stats.mu.Lock()
	defer stats.mu.Unlock()
	require.Len(t, stats.buckets, 1, "billed 日志进 StatBucket（评审 M-3）")
	b := stats.buckets[0]
	require.Equal(t, int64(1), b.RequestCount)
	require.Equal(t, int64(130), b.Cost)
	require.Equal(t, int64(3), b.InputTokens)
	require.Equal(t, int64(5), b.OutputTokens)
	require.Equal(t, "gpt-4o", b.Model)
	require.Equal(t, int64(1), b.UserID)
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
// - 预算内完成：Close 实际等待（不提前返回），完整排空，无截断 Warn；
// - 预算到期：Cancel baseCtx → 在途 DeductAndLog 快速失败（未落库、回灌不
//   丢）→ 截断 Warn（flushed/remaining 条数）+ 快速退出（不等其自然完成）。
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
	f.flush() // 回落复位（pendingN < 水线 → warned 复位）
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
