// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

// err_logs worker 单测（架构审查 C28）：有界队列满非阻塞丢弃 + 计数、B2 双队列
// 豁免（拒绝风暴不丢双轨行）、flush 批界、Close 完整排空/预算截断、InsertBatch
// 失败止损丢弃、S4 尾窗口竞态、告警边沿（S3）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// captureErrLogInserter 记录 InsertBatch 批（批/字段断言）。
type captureErrLogInserter struct {
	mu      sync.Mutex
	batches [][]*domain.UsageLog
	fail    bool // true = 注入失败
}

func (c *captureErrLogInserter) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("injected insert failure")
	}
	c.batches = append(c.batches, logs)
	return nil
}

func (c *captureErrLogInserter) rows() []*domain.UsageLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*domain.UsageLog
	for _, b := range c.batches {
		out = append(out, b...)
	}
	return out
}

func (c *captureErrLogInserter) batchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.batches)
}

func rejectLog(i int) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: "rej-" + string(rune('a'+i%26)),
		Model:     "m", Format: domain.FormatOpenAIChat,
		StatusCode: 429, ErrorType: domain.Err429, CreatedAt: time.Now(),
	}
}

func errorLog(i int) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: "err-" + string(rune('a'+i%26)),
		Model:     "m", Format: domain.FormatOpenAIChat,
		StatusCode: 502, ErrorType: domain.Err5xx, CreatedAt: time.Now(),
	}
}

func newTestErrLogWorker(writer ErrLogInserter, queueSize int) *ErrLogWorker {
	return NewErrLogWorker(ErrLogConfig{
		QueueSize: queueSize, FlushInterval: time.Hour,
	}, writer, nil)
}

// TestErrLogWorkerPersists 正常落盘：拒绝 + 双轨行混合入队 → Close 完整排空，
// 字段保留（request_id/error_type/status/错误文本）。
func TestErrLogWorkerPersists(t *testing.T) {
	store := &captureErrLogInserter{}
	w := newTestErrLogWorker(store, 64)
	msg := "key quota exhausted"
	l := rejectLog(0)
	l.ErrorMessage = &msg
	w.EnqueueRejected(l)
	w.EnqueueRejected(rejectLog(1))
	w.EnqueueError(errorLog(2))
	w.EnqueueError(errorLog(3))

	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, int64(4), w.Inserted())
	require.Zero(t, w.DroppedReject()+w.DroppedExempt(), "无丢弃")
	rows := store.rows()
	require.Len(t, rows, 4)
	got := map[string]*domain.UsageLog{}
	for _, r := range rows {
		got[r.RequestID] = r
	}
	require.NotNil(t, got["rej-a"].ErrorMessage)
	require.Equal(t, msg, *got["rej-a"].ErrorMessage, "错误文本保留")
	require.Equal(t, domain.Err429, got["rej-a"].ErrorType)
	require.Equal(t, 429, got["rej-a"].StatusCode)
	require.Equal(t, domain.Err5xx, got["err-c"].ErrorType)
	require.Equal(t, 502, got["err-c"].StatusCode)
}

// TestErrLogWorkerRejectStormSamplesAndExemptSurvives B2 核心：拒绝风暴灌满
// 普通队列 → 拒绝行采样丢弃（非阻塞、计数正确、队列有界）——双轨行走豁免
// 队列**不丢**（无消费方下 EnqueueError 全部入队）。
func TestErrLogWorkerRejectStormSamplesAndExemptSurvives(t *testing.T) {
	store := &captureErrLogInserter{}
	w := newTestErrLogWorker(store, 64) // 小队列放大风暴效应；无消费方（不 Start）

	const rejectN = 100_000
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < rejectN; i++ {
			w.EnqueueRejected(rejectLog(i))
		}
	}()
	select { // 热路径不阻塞（B2/背压核心）
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("EnqueueRejected 风暴必须非阻塞完成（有界队列 select-default）")
	}

	// 双轨行（豁免通道）恒入队：无消费方 + 风暴后仍全部进豁免队列
	for i := 0; i < 100; i++ {
		w.EnqueueError(errorLog(i))
	}
	require.Equal(t, int64(rejectN-64), w.DroppedReject(), "拒绝行丢弃计数 = 到达 - 队列容量")
	require.Zero(t, w.DroppedExempt(), "双轨行零丢弃（豁免）")
	require.Equal(t, 64, len(w.rejectQ), "拒绝队列有界（内存稳定）")
	require.Equal(t, 100, len(w.exemptQ), "豁免队列全量入队（不参与采样）")
	require.Equal(t, 164, w.Queued())

	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, int64(64+100), w.Inserted(), "队列内拒绝行 + 全部双轨行完整排空")
	require.Zero(t, w.Queued())
}

// TestErrLogWorkerBatches 批界：单批 ≤ BatchSize（Close 排空逐批）。
func TestErrLogWorkerBatches(t *testing.T) {
	store := &captureErrLogInserter{}
	w := NewErrLogWorker(ErrLogConfig{QueueSize: 4096, BatchSize: 50, FlushInterval: time.Hour}, store, nil)
	for i := 0; i < 120; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, 3, store.batchCount(), "120 行 / 50 批界 = 3 批")
	for _, b := range store.batches {
		require.LessOrEqual(t, len(b), 50, "单批 ≤ BatchSize")
	}
}

// slowErrLogInserter 慢落库（每批 sleep——预算截断路径模拟）。
type slowErrLogInserter struct {
	latency time.Duration
}

func (s *slowErrLogInserter) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.latency):
	}
	return nil
}

// TestErrLogWorkerCloseTruncatesOnBudget 停机排空受预算约束：deadline 到期 →
// 截断退出 + Warn（flushed/remaining），不无阻塞停机。
func TestErrLogWorkerCloseTruncatesOnBudget(t *testing.T) {
	w := NewErrLogWorker(ErrLogConfig{QueueSize: 4096, BatchSize: 100, FlushInterval: time.Hour},
		&slowErrLogInserter{latency: 30 * time.Millisecond}, nil)
	for i := 0; i < 1000; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	logger, out := newTestErrLogLogger(t)
	w.log = logger

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Millisecond)
	defer cancel()
	require.NoError(t, w.Close(ctx))
	require.Greater(t, w.Inserted(), int64(0), "预算内尽量排空（至少一批）")
	require.Greater(t, w.DroppedReject()+w.DroppedExempt(), int64(0),
		"截断剩余计入丢弃计数（落库失败/截断批并入类计数——对账指标）")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "shutdown budget exceeded, truncated drain")
}

// TestErrLogWorkerCloseTruncationCountsQueueBacklog R2-C1 精确回归：预算已到期
// 时 Close 立即截断——截断面 = 本批（BatchSize 100，双轨优先：50 双轨 + 50 拒绝）
// + 两队列剩余积压（拒绝 200），全部并入 dropped 对账指标，不低估。
func TestErrLogWorkerCloseTruncationCountsQueueBacklog(t *testing.T) {
	w := NewErrLogWorker(ErrLogConfig{QueueSize: 4096, BatchSize: 100, FlushInterval: time.Hour},
		&captureErrLogInserter{}, nil)
	for i := 0; i < 250; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	for i := 0; i < 50; i++ {
		w.EnqueueError(errorLog(i))
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.NoError(t, w.Close(ctx))
	require.Zero(t, w.Inserted(), "预算已到期：无任何落库")
	require.Equal(t, int64(100), w.DroppedExempt(), "本批 100（50 双轨 + 50 拒绝）按 dropBatch 既有语义计入双轨丢弃")
	require.Equal(t, int64(200), w.DroppedReject(), "R2-C1：拒绝队列剩余积压并入丢弃计数（对账不低估）")
}

// TestErrLogWorkerInsertFailureDrops InsertBatch 失败 → 整批丢弃止损 + 计数
// （审计明细非计费：不无限回灌卡死）。
func TestErrLogWorkerInsertFailureDrops(t *testing.T) {
	store := &captureErrLogInserter{fail: true}
	w := newTestErrLogWorker(store, 64)
	w.EnqueueError(errorLog(1))
	w.EnqueueRejected(rejectLog(1))
	require.NoError(t, w.Close(context.Background()))
	require.Zero(t, w.Inserted())
	require.Equal(t, int64(2), w.DroppedExempt(), "失败批并入丢弃计数（对账指标）")
}

// TestErrLogWorkerCloseTailWindowNoSilentLoss S4：Close 与 Enqueue 并发——置位
// closed 后无尾窗口静默丢（inserted + dropped == 全部投递）。
func TestErrLogWorkerCloseTailWindowNoSilentLoss(t *testing.T) {
	store := &captureErrLogInserter{}
	w := newTestErrLogWorker(store, 4096)

	const total = 20_000
	enqueued := atomic.Int64{}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(4)
	for g := 0; g < 4; g++ {
		go func(g int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
					w.EnqueueRejected(rejectLog(g*1000 + i))
					enqueued.Add(1)
					i++
				}
			}
		}(g)
	}
	// 风暴投递同时 Close（预算充足——排空应覆盖全部已入队行）
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, w.Close(ctx))
	close(stop)
	wg.Wait()

	accounted := w.Inserted() + w.DroppedReject() + w.DroppedExempt()
	require.Equal(t, enqueued.Load(), accounted,
		"无尾窗口静默丢：inserted+dropped == 全部投递（S4）")
	require.Zero(t, w.Queued(), "排空后队列空")
}

// TestErrLogWorkerDropWarnEdge S3：丢弃累计 ≥ 阈值 → Warn 恰好一次；队列排空
// （flush 空批）边沿回落 → 再次风暴再次 Warn（每风暴一次，不刷屏）。
func TestErrLogWorkerDropWarnEdge(t *testing.T) {
	old := errlogDropWarnThreshold
	errlogDropWarnThreshold = 50
	t.Cleanup(func() { errlogDropWarnThreshold = old })

	store := &captureErrLogInserter{}
	w := newTestErrLogWorker(store, 8)
	logger, out := newTestErrLogLogger(t)
	w.log = logger

	// 第一轮风暴（无消费方）→ 一次 Warn
	for i := 0; i < 100; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	w.flush() // 排空队列 → 告警边沿回落
	// 第二轮风暴 → 再次 Warn
	for i := 0; i < 100; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	w.flush()
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(b), "errlog queue full, dropping"),
		"每风暴一次 Warn（边沿回落）")
	require.NoError(t, w.Close(context.Background()))
}

// newTestErrLogLogger warn 级文件 logger（Warn 断言用；Windows 句柄 best-effort）。
func newTestErrLogLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "errlog-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New("warn", out)
	require.NoError(t, err)
	return logger, out
}
