package billing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/usage"
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

// concWriter 记录并发调用峰值（分片并行断言）。
type concWriter struct {
	mu        sync.Mutex
	calls     []deductCall
	cur, max  int
}

func (w *concWriter) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	w.mu.Lock()
	w.cur++
	if w.cur > w.max {
		w.max = w.cur
	}
	w.mu.Unlock()
	time.Sleep(20 * time.Millisecond) // 放大重叠窗口
	w.mu.Lock()
	w.cur--
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
// 峰值并发 = worker 数（8 user / 4 worker），每用户恰好一笔（同 user 恒同桶）。
func TestFlusherParallelWorkers(t *testing.T) {
	writer := &concWriter{}
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
		PromptTokens: 3, CompletionTokens: 5, Cost: 130,
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
	require.Equal(t, int64(3), b.PromptTokens)
	require.Equal(t, int64(5), b.CompletionTokens)
	require.Equal(t, "gpt-4o", b.Model)
	require.Equal(t, int64(1), b.UserID)
}
