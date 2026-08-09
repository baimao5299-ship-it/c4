package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

type memLogStore struct {
	mu   sync.Mutex
	logs []*domain.UsageLog
}

func (m *memLogStore) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, logs...)
	return nil
}

type memStatStore struct {
	mu      sync.Mutex
	buckets []*domain.StatBucket
}

func (m *memStatStore) Upsert(ctx context.Context, b []*domain.StatBucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, nb := range b {
		for _, ob := range m.buckets {
			if ob.BucketTime.Equal(nb.BucketTime) && ob.GroupID == nb.GroupID && ob.Model == nb.Model && ob.IsError == nb.IsError {
				ob.RequestCount += nb.RequestCount
				ob.ErrorCount += nb.ErrorCount
				ob.TotalTokens += nb.TotalTokens
				ob.CacheReadTokens += nb.CacheReadTokens
				ob.CacheCreationTokens += nb.CacheCreationTokens
				ob.TotalLatencyMS += nb.TotalLatencyMS
				goto next
			}
		}
		m.buckets = append(m.buckets, nb)
	next:
	}
	return nil
}

func testCfg() UsageConfig {
	return UsageConfig{
		BatchSize:          2,
		FlushInterval:      50 * time.Millisecond,
		StatsFlushInterval: 30 * time.Millisecond,
	}
}

func TestRecorderFlushesLogs(t *testing.T) {
	ls := &memLogStore{}
	ss := &memStatStore{}
	r := New(testCfg(), ls, ss, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, r.Start(ctx))

	r.Record(&domain.UsageLog{RequestID: "a", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 10, CreatedAt: time.Now()})
	r.Record(&domain.UsageLog{RequestID: "b", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 20, CreatedAt: time.Now()})

	// 等批量刷出
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ls.mu.Lock()
		n := len(ls.logs)
		ls.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ls.mu.Lock()
	n := len(ls.logs)
	ls.mu.Unlock()
	require.GreaterOrEqual(t, n, 2, "logs flushed")
	cancel()
	r.Close(context.Background())
}

func TestRecorderAggregatesStats(t *testing.T) {
	ls := &memLogStore{}
	ss := &memStatStore{}
	r := New(testCfg(), ls, ss, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, r.Start(ctx))

	now := time.Now().Truncate(time.Hour)
	r.Record(&domain.UsageLog{RequestID: "a", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 10, LatencyMS: 5, CacheReadTokens: 4, CacheCreationTokens: 2, Cost: 100, CreatedAt: now})
	r.Record(&domain.UsageLog{RequestID: "b", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 500, ErrorType: domain.Err5xx, LatencyMS: 7, CreatedAt: now})
	r.Record(&domain.UsageLog{RequestID: "c", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 30, LatencyMS: 9, CacheReadTokens: 6, CacheCreationTokens: 3, Cost: 50, CreatedAt: now})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ss.mu.Lock()
		flushed := len(ss.buckets) >= 2
		ss.mu.Unlock()
		if flushed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	require.Len(t, ss.buckets, 2, "want 2 buckets (ok/err)")
	var okB, errB *domain.StatBucket
	for _, b := range ss.buckets {
		if b.IsError {
			errB = b
		} else {
			okB = b
		}
	}
	require.NotNil(t, okB)
	require.Equal(t, int64(2), okB.RequestCount)
	require.Equal(t, int64(40), okB.TotalTokens)
	require.Equal(t, int64(10), okB.CacheReadTokens, "cache read SUM 进聚合桶")
	require.Equal(t, int64(5), okB.CacheCreationTokens, "cache creation SUM 进聚合桶")
	require.Equal(t, int64(150), okB.Cost, "cost SUM 进聚合桶（100+50）")
	require.NotNil(t, errB)
	require.Equal(t, int64(1), errB.RequestCount)
	require.Equal(t, int64(1), errB.ErrorCount)
	require.Zero(t, errB.CacheReadTokens, "无缓存记录 → 0")
	cancel()
	r.Close(context.Background())
}

// failStatStore 模拟 Upsert 失败（flushStats 回灌路径，评审 M3）。
type failStatStore struct {
	fail    bool
	buckets []*domain.StatBucket
}

func (m *failStatStore) Upsert(ctx context.Context, b []*domain.StatBucket) error {
	if m.fail {
		return errors.New("db down")
	}
	m.buckets = append(m.buckets, b...)
	return nil
}

// TestFlushStatsRefeedsCacheTokens 失败回灌：Upsert 失败后计数回灌内存计数，
// 下一次成功 flush 聚合不丢 cache 字段（评审 M3 两处 SUM）。
func TestFlushStatsRefeedsCacheTokens(t *testing.T) {
	ls := &memLogStore{}
	ss := &failStatStore{}
	r := New(UsageConfig{BatchSize: 10}, ls, ss, nil)

	now := time.Now().Truncate(time.Hour)
	r.Record(&domain.UsageLog{RequestID: "a", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 10, CacheReadTokens: 4, CacheCreationTokens: 2, Cost: 5, CreatedAt: now})
	r.Record(&domain.UsageLog{RequestID: "c", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 30, CacheReadTokens: 6, CacheCreationTokens: 3, Cost: 15, CreatedAt: now})

	// 第一次 flush 失败 → 计数回灌
	ss.fail = true
	r.flushStats(context.Background())
	require.Empty(t, ss.buckets)

	// 第二次成功 flush → 回灌的 cache 计数不丢
	ss.fail = false
	r.flushStats(context.Background())
	require.Len(t, ss.buckets, 1)
	require.Equal(t, int64(10), ss.buckets[0].CacheReadTokens, "回灌后 cache read 不丢")
	require.Equal(t, int64(5), ss.buckets[0].CacheCreationTokens, "回灌后 cache creation 不丢")
	require.Equal(t, int64(20), ss.buckets[0].Cost, "回灌后 cost 不丢")
	require.Equal(t, int64(2), ss.buckets[0].RequestCount)
}

// TestAggregateSkipsLogChannel Aggregate 只聚合统计（含 cost 进 StatBucket），
// 不入明细 channel——T3 计费 Flusher 复用同一聚合（每日志恰好一个写者）。
func TestAggregateSkipsLogChannel(t *testing.T) {
	ls := &memLogStore{}
	ss := &memStatStore{}
	r := New(testCfg(), ls, ss, nil)
	now := time.Now().Truncate(time.Hour)
	r.Aggregate(&domain.UsageLog{RequestID: "a", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 10, Cost: 123, CreatedAt: now})
	require.Zero(t, r.Pending(), "Aggregate 不得入明细 channel")
	r.flushStats(context.Background())
	require.Len(t, ss.buckets, 1)
	require.Equal(t, int64(1), ss.buckets[0].RequestCount)
	require.Equal(t, int64(10), ss.buckets[0].TotalTokens)
	require.Equal(t, int64(123), ss.buckets[0].Cost, "cost 进 StatBucket")
	require.Empty(t, ls.logs, "Aggregate 不落明细")
}

func TestRecordBackpressureWhenFull(t *testing.T) {
	ls := &memLogStore{}
	ss := &memStatStore{}
	r := New(testCfg(), ls, ss, nil)

	// 填满有界 channel（cap 16384）：饱和后 Record 必须阻塞反压、绝不丢数据
	// （用户决策 2026-08-05：DropOnFull 已移除）。
	log := &domain.UsageLog{RequestID: "bp", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 1, CreatedAt: time.Now()}
	for i := 0; i < cap(r.logCh); i++ {
		r.logCh <- log
	}

	returned := make(chan struct{})
	go func() {
		r.Record(log)
		close(returned)
	}()

	// 满时 Record 应阻塞：50ms 内不得返回
	select {
	case <-returned:
		require.Fail(t, "Record returned while channel full; want blocking backpressure")
	case <-time.After(50 * time.Millisecond):
	}

	// 腾出一个槽位后 Record 应立刻完成发送
	<-r.logCh
	select {
	case <-returned:
	case <-time.After(time.Second):
		require.Fail(t, "Record still blocked after a slot was freed")
	}
}

// fakeQuotaWriter 记录 AddQuotaUsed 调用；cancel 非 nil 时首调触发（模拟预算
// 在第一个 key 写完后到期）——确定性截断，无时间依赖。
type fakeQuotaWriter struct {
	mu     sync.Mutex
	calls  []int64 // 回写过的 key
	n      int
	cancel context.CancelFunc
}

func (q *fakeQuotaWriter) AddQuotaUsed(ctx context.Context, deltas map[int64]int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.n++
	if q.n == 1 && q.cancel != nil {
		q.cancel() // 后续迭代查 ctx.Err() → 截断
	}
	for k, d := range deltas {
		q.calls = append(q.calls, k*1000+d)
	}
	return nil
}

// usageTestLogger warn 级文件 logger（Warn 断言用）。
func usageTestLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "usage-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New("warn", out)
	require.NoError(t, err)
	return logger, out
}

// TestFlushStatsTruncatesOnBudget O2 停机修复：flushStats 受 ctx 预算约束。
// 预算到期（首 key 写完后取消）→ 逐 key 检查截断退出 + Warn（含已刷/剩余
// key 数）+ 统计桶一并截断 Warn；正常（无 deadline）→ 全量回写 + Upsert，
// 无 Warn。
func TestFlushStatsTruncatesOnBudget(t *testing.T) {
	newRec := func(q *fakeQuotaWriter, log *logx.Logger) *Recorder {
		r := New(UsageConfig{BatchSize: 100}, &memLogStore{}, &memStatStore{}, log)
		if q != nil {
			r.SetQuotaWriter(q)
		}
		return r
	}
	now := time.Now().Truncate(time.Hour)
	rec := func(r *Recorder, keyID int64) {
		r.Aggregate(&domain.UsageLog{RequestID: "x", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 1, KeyID: keyID, CreatedAt: now})
	}

	t.Run("truncates on budget expiry", func(t *testing.T) {
		logger, out := usageTestLogger(t)
		ctx, cancel := context.WithCancel(context.Background())
		q := &fakeQuotaWriter{cancel: cancel}
		r := newRec(q, logger)
		for i := int64(1); i <= 3; i++ {
			rec(r, i) // 3 个额度 key + 1 个统计桶
		}
		r.flushStats(ctx)

		q.mu.Lock()
		written := len(q.calls)
		q.mu.Unlock()
		require.Equal(t, 1, written, "首 key 写完后预算到期，其余截断")
		require.Len(t, r.quotaUsed, 0, "截断丢弃剩余额度增量（不落库不回灌）")
		require.NoError(t, logger.Sync())
		b, err := os.ReadFile(out)
		require.NoError(t, err)
		require.Contains(t, string(b), "usage quota flush truncated on shutdown budget")
		require.Contains(t, string(b), `"quota_flushed_keys":1`)
		require.Contains(t, string(b), `"quota_remaining_keys":2`)
		require.Contains(t, string(b), "usage stats flush truncated on shutdown budget")
		require.Contains(t, string(b), `"stats_remaining_buckets":1`)
	})

	t.Run("flushes fully without deadline", func(t *testing.T) {
		logger, out := usageTestLogger(t)
		q := &fakeQuotaWriter{}
		r := newRec(q, logger)
		ss := &memStatStore{}
		r.stats = ss
		for i := int64(1); i <= 3; i++ {
			rec(r, i)
		}
		r.flushStats(context.Background())

		q.mu.Lock()
		written := len(q.calls)
		q.mu.Unlock()
		require.Equal(t, 3, written, "无 deadline 全量回写")
		ss.mu.Lock()
		buckets := len(ss.buckets)
		ss.mu.Unlock()
		require.Equal(t, 1, buckets, "统计桶正常 Upsert")
		require.NoError(t, logger.Sync())
		b, err := os.ReadFile(out)
		require.NoError(t, err)
		require.NotContains(t, string(b), "truncated on shutdown budget", "正常路径无截断 Warn")
	})
}
