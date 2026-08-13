// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
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

// failStatStore 模拟 Upsert 失败（flushStats 回灌路径，评审 M3）；并发安全
// （flushStats 多 worker 并行调用）。
type failStatStore struct {
	mu      sync.Mutex
	fail    bool
	buckets []*domain.StatBucket
}

func (m *failStatStore) Upsert(ctx context.Context, b []*domain.StatBucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	ss.mu.Lock()
	require.Empty(t, ss.buckets)
	ss.mu.Unlock()

	// 第二次成功 flush → 回灌的 cache 计数不丢
	ss.fail = false
	r.flushStats(context.Background())
	ss.mu.Lock()
	defer ss.mu.Unlock()
	require.Len(t, ss.buckets, 1)
	require.Equal(t, int64(10), ss.buckets[0].CacheReadTokens, "回灌后 cache read 不丢")
	require.Equal(t, int64(5), ss.buckets[0].CacheCreationTokens, "回灌后 cache creation 不丢")
	require.Equal(t, int64(20), ss.buckets[0].Cost, "回灌后 cost 不丢")
	require.Equal(t, int64(2), ss.buckets[0].RequestCount)
}

// blockingStatStore Upsert 首调阻塞至 release（模拟统计 flush 在途——桶已
// swap、锁已释放；A-P2-8-3 O(1) 交换后并发正确性测试用）。
type blockingStatStore struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	buckets []*domain.StatBucket
}

func (m *blockingStatStore) Upsert(ctx context.Context, b []*domain.StatBucket) error {
	select {
	case m.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.release:
		m.mu.Lock()
		m.buckets = append(m.buckets, b...)
		m.mu.Unlock()
		return nil
	}
}

// TestFlushStatsSwapConcurrentCorrect A-P2-8-3/P3-4：flushStats 锁内 O(1) 换引用
// 后并发正确性——统计 flush 在途（桶已 swap、锁已释放）时并发 Record 聚合进
// 新 counters map；两轮 flush 合并断言桶计数不丢不重（换出后旧 map 无写者，
// 写者只碰新 map——锁内仅剩 O(1) 聚合 + 换引用）。同时覆盖 Record 单临界区
// （P3-4 合并双锁后行为不变：聚合 + pending append 一把锁）。
func TestFlushStatsSwapConcurrentCorrect(t *testing.T) {
	ss := &blockingStatStore{started: make(chan struct{}), release: make(chan struct{})}
	r := New(UsageConfig{BatchSize: 10}, &memLogStore{}, ss, nil)
	now := time.Now().Truncate(time.Hour)
	rec := func(i int) *domain.UsageLog {
		return &domain.UsageLog{RequestID: fmt.Sprintf("r-%d", i), GroupID: 1, Model: "m",
			Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone,
			TotalTokens: 1, CreatedAt: now}
	}
	for i := 0; i < 100; i++ {
		r.Aggregate(rec(i)) // 首批：桶 A 计数 100
	}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		r.flushStats(context.Background()) // 首批在途（桶已 swap，锁已释放）
	}()
	<-ss.started

	// flush 在途期间并发 Record：聚合进新 counters（O(1) 交换后锁内无遍历，
	// Record 不阻塞；单临界区合并双锁后行为不变）
	for i := 0; i < 100; i++ {
		r.Record(rec(i))
	}
	close(ss.release)
	<-firstDone

	r.flushStats(context.Background()) // 第二批：在途期间聚合的桶
	ss.mu.Lock()
	defer ss.mu.Unlock()
	require.Len(t, ss.buckets, 2, "两轮 flush 各一个桶")
	var count int64
	for _, b := range ss.buckets {
		count += b.RequestCount
	}
	require.Equal(t, int64(200), count, "换批不丢不重（首批 100 + 在途期间 100）")
	require.Equal(t, 100, r.Pending(), "Record 明细留在 pending（本次未 flush 明细）")
}

// TestAggregateSkipsLogChannel Aggregate 只聚合统计（含 cost 进 StatBucket），
// 不入明细 pending——T3 计费 Flusher 复用同一聚合（每日志恰好一个写者）。
func TestAggregateSkipsLogChannel(t *testing.T) {
	ls := &memLogStore{}
	ss := &memStatStore{}
	r := New(testCfg(), ls, ss, nil)
	now := time.Now().Truncate(time.Hour)
	r.Aggregate(&domain.UsageLog{RequestID: "a", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 10, Cost: 123, CreatedAt: now})
	require.Zero(t, r.Pending(), "Aggregate 不得入明细 pending")
	r.flushStats(context.Background())
	require.Len(t, ss.buckets, 1)
	require.Equal(t, int64(1), ss.buckets[0].RequestCount)
	require.Equal(t, int64(10), ss.buckets[0].TotalTokens)
	require.Equal(t, int64(123), ss.buckets[0].Cost, "cost 进 StatBucket")
	require.Empty(t, ls.logs, "Aggregate 不落明细")
}

// TestRecordNeverBlocks O1 管道化核心：Record 无 channel、永不阻塞——旧实现
// 有界 channel cap 16384 饱和后 Record 阻塞发送（off 路径幽灵根因，O3 复测
// 定位：16.4k goroutine 卡 chan send、healthz inflight 31-33k @10k）。无消费
// 者（不 Start）时 pending 无界累积，30k 条（> 旧 cap）必须全部立即返回。
func TestRecordNeverBlocks(t *testing.T) {
	ls := &memLogStore{}
	ss := &memStatStore{}
	r := New(UsageConfig{BatchSize: 10}, ls, ss, nil)
	log := &domain.UsageLog{RequestID: "nb", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 1, CreatedAt: time.Now()}

	const n = 30000 // 旧 cap 16384 之上
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			r.Record(log)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Record 阻塞：off 路径幽灵根因回归")
	}
	require.Equal(t, n, r.Pending(), "无消费者时 pending 无界累积（不丢数据语义）")
}

// TestRecordConcurrentNeverBlocks 高并发 Record 无阻塞点（race 关键路径）：
// 32 goroutine 并发 Record（无消费者），全部返回即证明无 channel 饱和阻塞；
// 完成后断言条数不丢。
func TestRecordConcurrentNeverBlocks(t *testing.T) {
	ls := &memLogStore{}
	ss := &memStatStore{}
	r := New(UsageConfig{BatchSize: 10}, ls, ss, nil)
	log := &domain.UsageLog{RequestID: "c", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 1, CreatedAt: time.Now()}

	const g = 32
	const per = 2000
	var wg sync.WaitGroup
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				r.Record(log)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("并发 Record 阻塞")
	}
	require.Equal(t, g*per, r.Pending())
}

// TestRecordAfterCloseWarnsOnce Close 后 Record（防御性缺口，评审 I-4）：
// closed 标记生效——Warn 恰好一次（不刷屏）、明细不丢（仍聚合入 pending）、
// 保持非阻塞。worker 管理器顺序（先停 HTTP 再 Close）下正常停机不触发。
func TestRecordAfterCloseWarnsOnce(t *testing.T) {
	logger, out := usageTestLogger(t)
	r := New(testCfg(), &memLogStore{}, &memStatStore{}, logger)
	require.NoError(t, r.Close(context.Background()))

	r.Record(&domain.UsageLog{RequestID: "late", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 1, CreatedAt: time.Now()})
	r.Record(&domain.UsageLog{RequestID: "late2", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 1, CreatedAt: time.Now()})

	require.Equal(t, 2, r.Pending(), "Close 后 Record 不丢（驻留内存由 Warn 观测）")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, 1, countOccurrences(b, "usage record after close"), "Warn 恰好一次")
	require.Contains(t, string(b), `"request_id":"late"`, "Warn 含首次 Record 的 request_id")
}

// countLogStore 统计 InsertBatch 调用次数与落库条数（批量化断言）。
type countLogStore struct {
	mu    sync.Mutex
	calls int
	logs  []*domain.UsageLog
}

func (m *countLogStore) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.logs = append(m.logs, logs...)
	return nil
}

// TestLogFlushBatchedWrites 批量化：1000 条明细 / BatchSize 500 → 恰好 2 次
// InsertBatch（旧实现逐条 channel 消费同样批量，此处断言批量写次数受控）。
func TestLogFlushBatchedWrites(t *testing.T) {
	ls := &countLogStore{}
	ss := &memStatStore{}
	r := New(UsageConfig{BatchSize: 500, Workers: 1}, ls, ss, nil)
	now := time.Now()
	for i := 0; i < 1000; i++ {
		r.Record(&domain.UsageLog{RequestID: "x", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: now})
	}
	flushed := r.flushLogs(context.Background())
	require.Equal(t, int64(1000), flushed)
	ls.mu.Lock()
	defer ls.mu.Unlock()
	require.Equal(t, 2, ls.calls, "1000 条 / 500 = 2 次批量写")
	require.Len(t, ls.logs, 1000)
	require.Zero(t, r.Pending())
}

// TestLogFlushParallelWorkers 并行分片：4 worker 下 1000 条全部落库（分片后
// 同 user 恒同 worker，跨 worker 无重复无丢失）。
func TestLogFlushParallelWorkers(t *testing.T) {
	ls := &countLogStore{}
	ss := &memStatStore{}
	r := New(UsageConfig{BatchSize: 100, Workers: 4}, ls, ss, nil)
	now := time.Now()
	for i := 0; i < 1000; i++ {
		r.Record(&domain.UsageLog{RequestID: "x", UserID: int64(i % 100), Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: now})
	}
	flushed := r.flushLogs(context.Background())
	require.Equal(t, int64(1000), flushed)
	ls.mu.Lock()
	defer ls.mu.Unlock()
	require.Len(t, ls.logs, 1000)
	require.Zero(t, r.Pending())
}

// failOnceLogStore 首次 InsertBatch 失败，其后成功（回灌重试路径）。
type failOnceLogStore struct {
	mu   sync.Mutex
	fail atomic.Bool
	logs []*domain.UsageLog
}

func (m *failOnceLogStore) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	if m.fail.CompareAndSwap(true, false) {
		return errors.New("db down")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, logs...)
	return nil
}

// TestLogRefillOnFailure 失败回灌：InsertBatch 失败 → 二分隔离（A-P2-8-4）后
// 未落库行回灌 pending 不丢；下次 flush 重试成功。瞬态失败（failOnce：仅首调
// 失败）时二分探测把失败 chunk 全部重试成功落库（部分成功语义：成功半照常
// 入库，不丢不重；失败半的残余回灌），其后剩余回灌——旧实现失败 chunk + 剩余
// 整体回灌 0 落库，回灌纪律不丢行不变，仅成功行提前落库。
func TestLogRefillOnFailure(t *testing.T) {
	ls := &failOnceLogStore{}
	ls.fail.Store(true)
	ss := &memStatStore{}
	r := New(UsageConfig{BatchSize: 100, Workers: 1}, ls, ss, nil)
	now := time.Now()
	for i := 0; i < 250; i++ {
		r.Record(&domain.UsageLog{RequestID: "x", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: now})
	}
	// 首次 flush：chunk1(100) 首调失败 → 二分探测重试全量成功（瞬态失败不丢行
	// 不重复），其后剩余 150 回灌 → 落库 100
	require.Equal(t, int64(100), r.flushLogs(context.Background()))
	require.Equal(t, 150, r.Pending(), "失败 chunk 后剩余回灌，不丢")
	// 第二次 flush：全部落库
	require.Equal(t, int64(150), r.flushLogs(context.Background()))
	ls.mu.Lock()
	require.Len(t, ls.logs, 250)
	ls.mu.Unlock()
	require.Zero(t, r.Pending())
}

// poisonRowLogStore 注入指定 request_id 的毒丸行（A-P2-8-4 二分定位对象）：含
// 毒丸行的批恒失败（模拟单行永久失败——约束冲突/畸形数据形态），其余批正常
// 入库。并发安全（flushLogs 多 worker 并行调用）。
type poisonRowLogStore struct {
	mu     sync.Mutex
	poison string
	logs   []*domain.UsageLog
}

func (m *poisonRowLogStore) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range logs {
		if l.RequestID == m.poison {
			return errors.New("injected poison row failure")
		}
	}
	m.logs = append(m.logs, logs...)
	return nil
}

// TestLogPoisonRowIsolatedByBisect 毒丸止损二分隔离（A-P2-8-4）：单行毒丸（旧
// 实现整 chunk 丢弃，单行毒丸连带 499 行有效明细）——失败路径二分重试定位
// 毒丸行 → 仅丢弃该行（Error + request_id + dropped_logs=1）、其余行全部成功
// 落库（"其余入库"）、计数复位、无回灌残留。
func TestLogPoisonRowIsolatedByBisect(t *testing.T) {
	logger, out := usageTestLogger(t)
	ls := &poisonRowLogStore{poison: "poison-3"}
	r := New(UsageConfig{BatchSize: 8, Workers: 1}, ls, &memStatStore{}, logger)
	now := time.Now()
	for i := 0; i < 8; i++ {
		req := fmt.Sprintf("req-%d", i)
		if i == 3 {
			req = "poison-3"
		}
		r.Record(&domain.UsageLog{RequestID: req, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: now})
	}

	flushed := r.flushLogs(context.Background())
	require.Equal(t, int64(7), flushed, "毒丸行外 7 行由二分过程全部落库")
	ls.mu.Lock()
	require.Len(t, ls.logs, 7, "其余行全部入库（不再整 chunk 连带丢弃）")
	ls.mu.Unlock()
	require.Zero(t, r.Pending(), "毒丸行丢弃、其余入库，无回灌残留")
	require.Zero(t, r.failCounts[0], "毒丸定位隔离 → 计数复位")

	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "usage batch insert failed, dropping poison row")
	require.Contains(t, string(b), `"level":"error"`, "毒丸止损升级 Error 级")
	require.Contains(t, string(b), `"request_id":"poison-3"`, "Error 日志含毒丸行 request_id")
	require.Contains(t, string(b), `"dropped_logs":1`, "仅丢弃单行")
}

// dbDownLogStore 可切换整库故障的 InsertBatch（A-P2-8-4 整库故障形态）：fail=
// true 恒失败（含二分探测——两半都失败），fail=false 正常入库——模拟 DB 恢复
// 后重试成功。并发安全。
type dbDownLogStore struct {
	mu   sync.Mutex
	fail bool
	logs []*domain.UsageLog
}

func (m *dbDownLogStore) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("db down")
	}
	m.logs = append(m.logs, logs...)
	return nil
}

// TestLogDBFailureRefillsNoProgressiveDrop 整库故障二分归因（A-P2-8-4）：两半都
// 失败 → 未落库行全部回灌（不丢）+ 不累计失败计数——故障期无进行式丢弃（旧
// 实现每 5 周期丢 1 chunk/分片 ≈ 24 万行蒸发）；DB 恢复即重试成功（成功路径
// 计数复位）。
func TestLogDBFailureRefillsNoProgressiveDrop(t *testing.T) {
	logger, out := usageTestLogger(t)
	ls := &dbDownLogStore{fail: true}
	r := New(UsageConfig{BatchSize: 4, Workers: 1}, ls, &memStatStore{}, logger)
	now := time.Now()
	for i := 0; i < 4; i++ {
		r.Record(&domain.UsageLog{RequestID: fmt.Sprintf("req-%d", i), Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: now})
	}

	// 故障期多轮 flush（6 轮 > 旧阈值 5，旧实现此处已丢弃 chunk）：全部回灌不丢
	// + 失败计数不累计（无任何触发丢弃）
	for i := 0; i < 6; i++ {
		require.Zero(t, r.flushLogs(context.Background()))
		require.Equal(t, 4, r.Pending(), "整库故障回灌不丢（无进行式丢弃）")
		require.Zero(t, r.failCounts[0], "整库故障不累计失败计数")
	}
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "usage batch insert failed (DB-wide), refilled")
	require.NotContains(t, string(b), "dropping poison", "故障期无任何丢弃（不区分失败原因的整 chunk 止损已废除）")

	// DB 恢复：重试成功全量落库（成功路径计数复位）
	ls.mu.Lock()
	ls.fail = false
	ls.mu.Unlock()
	require.Equal(t, int64(4), r.flushLogs(context.Background()))
	ls.mu.Lock()
	require.Len(t, ls.logs, 4)
	ls.mu.Unlock()
	require.Zero(t, r.Pending())
	require.Zero(t, r.failCounts[0])
}

// blockingLogStore 阻塞 InsertBatch（模拟慢 DB）：首调通知 started，release
// 放行；ctx 取消快速失败（Close 预算到期 Cancel baseCtx 路径）。
type blockingLogStore struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	logs    []*domain.UsageLog
}

func (m *blockingLogStore) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	select {
	case m.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.release:
		m.mu.Lock()
		m.logs = append(m.logs, logs...)
		m.mu.Unlock()
		return nil
	}
}

func (m *blockingLogStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.logs)
}

// TestCloseWaitsInflight O2 停机修复核心（对齐 billing Flusher 测试）：ticker
// 批次已在途（baseCtx、pending 已 swap、flushMu 被占）时 Close 必须先等其
// 结束——否则 drain 循环见 pendingN==0 静默提前返回，在途批次无界运行：
// - 预算内完成：Close 实际等待（不提前返回），完整排空，无截断 Warn；
// - 预算到期：Cancel baseCtx → 在途 InsertBatch 快速失败（未落库、回灌不
//   丢）→ 截断 Warn（flushed/remaining 条数）+ 快速退出（不等其自然完成）。
func TestCloseWaitsInflight(t *testing.T) {
	newRec := func(ls *blockingLogStore) *Recorder {
		r := New(UsageConfig{BatchSize: 10}, ls, &memStatStore{}, nil)
		r.Record(&domain.UsageLog{RequestID: "a", UserID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: time.Now()})
		r.Record(&domain.UsageLog{RequestID: "b", UserID: 2, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: time.Now()})
		return r
	}

	t.Run("waits within budget", func(t *testing.T) {
		ls := &blockingLogStore{started: make(chan struct{}), release: make(chan struct{})}
		r := newRec(ls)
		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			r.flushLogs(r.baseCtx) // ticker 路径批次（baseCtx）在途
		}()
		<-ls.started
		time.AfterFunc(500*time.Millisecond, func() { close(ls.release) })

		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2*time.Second))
		defer cancel()
		start := time.Now()
		closeDone := make(chan struct{})
		go func() {
			r.Close(ctx)
			close(closeDone)
		}()
		select {
		case <-closeDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Close 未在预算内返回（在途批次未被等待）")
		}
		elapsed := time.Since(start)
		require.GreaterOrEqual(t, elapsed, 400*time.Millisecond, "Close 必须等待在途批次完成（不得静默提前返回）")
		require.Less(t, elapsed, 1500*time.Millisecond, "在途批次自然完成后即返回（不得等满预算）")
		require.Equal(t, 2, ls.count(), "在途批次完整落库（无截断）")
		require.Zero(t, r.Pending(), "完整排空")
		<-flushDone
	})

	t.Run("cancels on budget expiry", func(t *testing.T) {
		ls := &blockingLogStore{started: make(chan struct{}), release: make(chan struct{})}
		r := newRec(ls)
		logger, out := usageTestLogger(t)
		r.log = logger
		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			r.flushLogs(r.baseCtx)
		}()
		<-ls.started

		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(150*time.Millisecond))
		defer cancel()
		start := time.Now()
		closeDone := make(chan struct{})
		go func() {
			r.Close(ctx)
			close(closeDone)
		}()
		select {
		case <-closeDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Close 未在预算内返回（在途批次取消未生效）")
		}
		require.Less(t, time.Since(start), 500*time.Millisecond, "在途批次必须被取消快速失败（不得等其自然完成）")
		require.Zero(t, ls.count(), "在途 InsertBatch 被取消——不得落库成功")
		require.Equal(t, 2, r.Pending(), "取消后回灌不丢")
		require.NoError(t, logger.Sync())
		b, err := os.ReadFile(out)
		require.NoError(t, err)
		require.Contains(t, string(b), "shutdown budget exceeded, truncated drain")
		require.Contains(t, string(b), `"flushed_logs":0`)
		require.Contains(t, string(b), `"remaining_logs":2`)
		<-flushDone
	})
}

// ignoreCtxLogStore InsertBatch 忽略 ctx 永久阻塞（模拟 DB 病态卡死——
// database/sql 取消路径本身被拖住的极端形态；A-P2-8-2 第二 select 兜底目标）。
// 测试结束即弃置（在途 goroutine 无放行通道，属刻意泄漏）。
type ignoreCtxLogStore struct {
	started chan struct{} // 首调已进入（测试等待在途批次）
}

func (m *ignoreCtxLogStore) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	select {
	case m.started <- struct{}{}:
	default:
	}
	<-make(chan struct{}) // 永久阻塞（不响应 ctx 取消；无发送者，永不返回）
	return nil
}

// TestCloseAbandonsInflightOnTimeout A-P2-8-2：`<-acquired` 第二 select 预算超时
// ——驱动不尊重 ctx（DB 病态卡死形态）时 Close 不再无界等待：预算到期 → Cancel
// baseCtx → 收尾宽限超时 → Warn 放弃排空、截断退出（在途批次由已取消 baseCtx
// 收尾回灌不丢——数据不因本超时而丢失；后续排空/统计收尾都被 flushMu 挡住，
// 不再触碰）。旧实现 `<-acquired` 无界等待，编排层强杀 → 全量内存 pending 丢失。
func TestCloseAbandonsInflightOnTimeout(t *testing.T) {
	logger, out := usageTestLogger(t)
	old := inflightAbandonGrace
	inflightAbandonGrace = 50 * time.Millisecond
	defer func() { inflightAbandonGrace = old }()

	ls := &ignoreCtxLogStore{started: make(chan struct{})}
	r := New(UsageConfig{BatchSize: 10}, ls, &memStatStore{}, logger)
	r.Record(&domain.UsageLog{RequestID: "a", UserID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: time.Now()})

	go r.flushLogs(r.baseCtx) // ticker 路径批次在途（永久阻塞，不响应取消）
	<-ls.started

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(100*time.Millisecond))
	defer cancel()
	start := time.Now()
	require.NoError(t, r.Close(ctx))
	require.Less(t, time.Since(start), 500*time.Millisecond, "放弃排空快速退出（不得无界等待在途批次）")

	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "usage close: in-flight flush not finished in time, abandoning drain")
	require.Contains(t, string(b), `"level":"warn"`)
}

// TestRecordDuringFlushNotBlocked flush 在途（DB 阻塞）时 Record 必须立即返回
// （旧实现 channel 被消费但 pending 换批后仍可能饱和——无界 pending 下永无
// 阻塞点）。
func TestRecordDuringFlushNotBlocked(t *testing.T) {
	ls := &blockingLogStore{started: make(chan struct{}), release: make(chan struct{})}
	ss := &memStatStore{}
	r := New(UsageConfig{BatchSize: 10}, ls, ss, nil)
	r.Record(&domain.UsageLog{RequestID: "a", UserID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: time.Now()})

	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		r.flushLogs(r.baseCtx)
	}()
	<-ls.started // flush 在途（pending 已 swap、worker 阻塞在 InsertBatch）

	recDone := make(chan struct{})
	go func() {
		r.Record(&domain.UsageLog{RequestID: "b", UserID: 2, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: time.Now()})
		close(recDone)
	}()
	select {
	case <-recDone:
	case <-time.After(time.Second):
		t.Fatal("flush 在途时 Record 阻塞")
	}
	close(ls.release)
	<-flushDone
	require.Equal(t, 1, r.Pending(), "flush 期间新 Record 进新 pending，下轮 flush 处理")
}

// fakeQuotaWriter 记录 AddQuotaUsed 调用；cancel 非 nil 时首调触发（模拟预算
// 在首组 key 写完后到期）——确定性截断，无时间依赖。
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
// 额度回写已批量化（quotaBatchSize 组 = 一条批量 SQL）——截断粒度从逐 key
// 变为逐组（组内单语句全成或全败，无部分状态；10k key 组数 ~20，预算检查点
// 不变）：首组写完后到期 → 额度全量已刷（单组）+ 统计桶截断 Warn；预算先期
// 到期 → 额度整批截断 Warn（不落库不回灌）。正常（无 deadline）→ 全量回写
// + Upsert，无 Warn。既有停机纪律（截断 + Warn + 不静默返回）不回退。
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

	t.Run("stats truncate after quota group written", func(t *testing.T) {
		logger, out := usageTestLogger(t)
		ctx, cancel := context.WithCancel(context.Background())
		q := &fakeQuotaWriter{cancel: cancel}
		r := newRec(q, logger)
		for i := int64(1); i <= 3; i++ {
			rec(r, i) // 3 个额度 key（单组）+ 1 个统计桶
		}
		r.flushStats(ctx)

		q.mu.Lock()
		written := len(q.calls)
		q.mu.Unlock()
		require.Equal(t, 3, written, "额度单组批量回写：组内全成（3 key 一次调用）")
		require.Len(t, r.quotaUsed, 0, "额度组已写，无残留")
		require.NoError(t, logger.Sync())
		b, err := os.ReadFile(out)
		require.NoError(t, err)
		require.Contains(t, string(b), "usage stats flush truncated on shutdown budget")
		require.Contains(t, string(b), `"stats_flushed_buckets":0`)
		require.Contains(t, string(b), `"stats_remaining_buckets":1`)
		require.NotContains(t, string(b), "usage quota flush truncated", "额度组内全成，无额度截断 Warn")
	})

	t.Run("quota truncates when budget pre-expired", func(t *testing.T) {
		logger, out := usageTestLogger(t)
		ctx, cancel := context.WithCancel(context.Background())
		q := &fakeQuotaWriter{}
		r := newRec(q, logger)
		for i := int64(1); i <= 3; i++ {
			rec(r, i)
		}
		cancel() // 预算先期到期（如 Close 已耗尽）→ 额度整批截断
		r.flushStats(ctx)

		q.mu.Lock()
		written := len(q.calls)
		q.mu.Unlock()
		require.Zero(t, written, "预算先期到期：额度不落库")
		require.Len(t, r.quotaUsed, 0, "截断丢弃剩余额度增量（不落库不回灌）")
		require.NoError(t, logger.Sync())
		b, err := os.ReadFile(out)
		require.NoError(t, err)
		require.Contains(t, string(b), "usage quota flush truncated on shutdown budget")
		require.Contains(t, string(b), `"quota_flushed_keys":0`)
		require.Contains(t, string(b), `"quota_remaining_keys":3`)
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

// countStatStore 统计 Upsert 调用次数（批量化断言）。
type countStatStore struct {
	mu      sync.Mutex
	calls   int
	buckets []*domain.StatBucket
}

func (m *countStatStore) Upsert(ctx context.Context, b []*domain.StatBucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.buckets = append(m.buckets, b...)
	return nil
}

// TestStatsFlushBatchedWrites 批量化核心：1100 个统计桶 → statBatchSize=500
// 分块 → 恰好 3 次 Upsert 调用（旧实现逐桶 1100 次）；1100 个额度 key →
// quotaBatchSize=500 → 恰好 3 次 AddQuotaUsed。
func TestStatsFlushBatchedWrites(t *testing.T) {
	t.Run("buckets chunked", func(t *testing.T) {
		ss := &countStatStore{}
		r := New(UsageConfig{BatchSize: 10, Workers: 1}, &memLogStore{}, ss, nil)
		now := time.Now().Truncate(time.Hour)
		for i := 0; i < 1100; i++ {
			r.Aggregate(&domain.UsageLog{RequestID: "x", GroupID: int64(i), Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 1, CreatedAt: now})
		}
		r.flushStats(context.Background())
		ss.mu.Lock()
		defer ss.mu.Unlock()
		require.Equal(t, 3, ss.calls, "1100 桶 / 500 = 3 次批量 Upsert（500/500/100）")
		require.Len(t, ss.buckets, 1100)
	})

	t.Run("quota chunked", func(t *testing.T) {
		q := &fakeQuotaWriter{}
		r := New(UsageConfig{BatchSize: 10}, &memLogStore{}, &memStatStore{}, nil)
		r.SetQuotaWriter(q)
		now := time.Now().Truncate(time.Hour)
		for i := int64(1); i <= 1100; i++ {
			r.Aggregate(&domain.UsageLog{RequestID: "x", GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 1, KeyID: i, CreatedAt: now})
		}
		r.flushStats(context.Background())
		q.mu.Lock()
		defer q.mu.Unlock()
		require.Equal(t, 3, q.n, "1100 key / 500 = 3 次 AddQuotaUsed（500/500/100）")
		require.Len(t, q.calls, 1100)
	})
}

// TestStatsFlushParallelWorkers 并行分片正确性：4 worker 下 500 个桶分片落库
// 无丢失（每桶恰好进一次 Upsert），聚合值完整。
func TestStatsFlushParallelWorkers(t *testing.T) {
	ss := &memStatStore{}
	r := New(UsageConfig{BatchSize: 10, Workers: 4}, &memLogStore{}, ss, nil)
	now := time.Now().Truncate(time.Hour)
	var wantTokens int64
	for i := 0; i < 500; i++ {
		tok := int64(i % 50)
		wantTokens += tok
		r.Aggregate(&domain.UsageLog{RequestID: "x", GroupID: int64(i), Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: tok, CreatedAt: now})
	}
	r.flushStats(context.Background())
	ss.mu.Lock()
	defer ss.mu.Unlock()
	require.Len(t, ss.buckets, 500)
	var got int64
	for _, b := range ss.buckets {
		got += b.TotalTokens
	}
	require.Equal(t, wantTokens, got, "分片并行落库无丢失无重复")
}

// TestShardForDeterministic swap/分片一致性：同 key 恒同 worker（FNV 哈希
// 确定性）——跨 flush 分片稳定，同桶永不跨 worker 拆分。
func TestShardForDeterministic(t *testing.T) {
	keys := []statBucketKey{
		{hourUnix: 1, groupID: 2, accountID: 3, templateID: 4, userID: 5, model: "m", isErr: false},
		{hourUnix: 1700000000, model: "", isErr: true},
		{hourUnix: 1700000000, model: "gpt-4o", isErr: true},
		{hourUnix: 1700000000, groupID: 7, accountID: 8, templateID: 9, userID: 10, model: "claude-3-5-sonnet-20241022", isErr: false},
	}
	for _, workers := range []int{1, 2, 3, 4, 8} {
		for _, k := range keys {
			first := shardFor(k, workers)
			require.GreaterOrEqual(t, first, 0)
			require.Less(t, first, workers)
			for i := 0; i < 100; i++ {
				require.Equal(t, first, shardFor(k, workers), "同 key 恒同 worker")
			}
		}
	}
}

// TestCloseDrainsFully 无 deadline 完整排空：明细 + 统计 + 额度全部落库，
// 无截断 Warn，不静默提前返回。
func TestCloseDrainsFully(t *testing.T) {
	logger, out := usageTestLogger(t)
	ls := &countLogStore{}
	ss := &memStatStore{}
	q := &fakeQuotaWriter{}
	r := New(UsageConfig{BatchSize: 500, Workers: 2}, ls, ss, logger)
	r.SetQuotaWriter(q)
	now := time.Now().Truncate(time.Hour)
	for i := 0; i < 1200; i++ {
		r.Record(&domain.UsageLog{RequestID: "x", UserID: int64(i % 50), GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 1, KeyID: 1, CreatedAt: now})
	}
	require.NoError(t, r.Close(context.Background()))

	ls.mu.Lock()
	require.Len(t, ls.logs, 1200, "明细完整排空")
	ls.mu.Unlock()
	ss.mu.Lock()
	require.Len(t, ss.buckets, 1)
	require.Equal(t, int64(1200), ss.buckets[0].RequestCount)
	ss.mu.Unlock()
	q.mu.Lock()
	require.Len(t, q.calls, 1, "额度回写 1200×1 token")
	q.mu.Unlock()
	require.Zero(t, r.Pending())
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.NotContains(t, string(b), "truncated", "完整排空无截断 Warn")
}

// TestCloseTruncatesOnBudget 预算到期：Close 截断退出 + Warn（flushed/
// remaining 条数单位一致）+ 统计面同样截断 Warn——不静默提前返回。
func TestCloseTruncatesOnBudget(t *testing.T) {
	logger, out := usageTestLogger(t)
	r := New(UsageConfig{BatchSize: 10, Workers: 1}, &countLogStore{}, &memStatStore{}, logger)
	now := time.Now().Truncate(time.Hour)
	for i := 0; i < 5; i++ {
		r.Record(&domain.UsageLog{RequestID: "x", UserID: 1, GroupID: 1, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 1, KeyID: 1, CreatedAt: now})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预算已到期
	start := time.Now()
	require.NoError(t, r.Close(ctx))
	require.Less(t, time.Since(start), 500*time.Millisecond, "预算到期快速退出")
	require.Equal(t, 5, r.Pending(), "截断丢弃剩余明细（崩溃等价语义，不落库）")

	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "shutdown budget exceeded, truncated drain")
	require.Contains(t, string(b), `"flushed_logs":0`)
	require.Contains(t, string(b), `"remaining_logs":5`)
	require.Contains(t, string(b), "usage stats flush truncated on shutdown budget")
}

// TestWaterlineWarns pending 水线 Warn（对齐 billing Flusher）：超阈值 →
// Warn；flush 回落复位后再超阈值再次 Warn。
func TestWaterlineWarns(t *testing.T) {
	logger, out := usageTestLogger(t)
	ls := &countLogStore{}
	r := New(UsageConfig{BatchSize: 100, Workers: 1}, ls, &memStatStore{}, logger)
	old := pendingWaterline
	pendingWaterline = 3
	defer func() { pendingWaterline = old }()

	now := time.Now()
	for i := 0; i < 4; i++ {
		r.Record(&domain.UsageLog{RequestID: "x", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: now})
	}
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "usage pending exceeds waterline")
	require.Contains(t, string(b), `"pending_logs":4`)

	// 回落复位后再超 → 再次 Warn
	r.flushLogs(context.Background())
	require.NoError(t, logger.Sync())
	b, err = os.ReadFile(out)
	require.NoError(t, err)
	n := countOccurrences(b, "usage pending exceeds waterline")
	require.Equal(t, 1, n)
	for i := 0; i < 4; i++ {
		r.Record(&domain.UsageLog{RequestID: "x", Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: now})
	}
	require.NoError(t, logger.Sync())
	b, err = os.ReadFile(out)
	require.NoError(t, err)
	n = countOccurrences(b, "usage pending exceeds waterline")
	require.Equal(t, 2, n, "回落复位后再超阈值再次 Warn")
}

func countOccurrences(b []byte, s string) int {
	n := 0
	for i := 0; i+len(s) <= len(b); {
		j := indexOf(b[i:], s)
		if j < 0 {
			break
		}
		n++
		i += j + len(s)
	}
	return n
}

func indexOf(b []byte, s string) int {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return i
		}
	}
	return -1
}
