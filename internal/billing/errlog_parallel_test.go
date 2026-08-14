// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// 分表设计并行隔离（用户裁决 2026-08-11）：计费 Flusher（billed 明细 →
// DeductAndLog）与 errlog worker（错误审计明细 → err_logs）在代理内并行运行，
// 两路无共享可变状态（Flusher pending map vs ErrLogWorker 队列）；同一
// Recorder 实例共享（billed 统计聚合面——每日志恰好一个统计写者）。并发喂入
// 两路 → 各自 Close 排空计数精确，互不串路、互不阻塞。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
)

// captureErrInserter errlog 写者捕获（InsertBatch 追加）。
type captureErrInserter struct {
	mu  sync.Mutex
	ids []string
}

func (c *captureErrInserter) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range logs {
		c.ids = append(c.ids, l.RequestID)
	}
	return nil
}

func (c *captureErrInserter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ids)
}

// TestFlusherErrlogWorkerParallelIsolated 并行隔离：同一 Recorder 上挂
// Flusher（billed）+ 并行 errlog worker（错误审计），多 goroutine 并发喂两路
// → 各自 Close 排空完整（flusher writer 调用 = billed 行数；errlog 捕获 =
// 拒绝行数），互不串路。
func TestFlusherErrlogWorkerParallelIsolated(t *testing.T) {
	writer := &fakeDeductWriter{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		StatsFlushInterval: time.Hour,
	}, noopLogInserter{}, nil)
	bal := NewBalances(fakeBalLoader{m: map[int64]int64{1: 1e9}}, nil)
	f := NewFlusher(FlushConfig{
		FlushInterval:          time.Hour,
		BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)

	errs := &captureErrInserter{}
	ew := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, errs, nil)

	const billed, rejected = 2000, 2000
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < billed/4; i++ {
				f.Record(&domain.UsageLog{
					RequestID: "billed", UserID: 1, Model: "m",
					ErrorType: domain.ErrNone, Cost: 100,
				})
			}
			for i := 0; i < rejected/4; i++ {
				ew.EnqueueRejected(&domain.UsageLog{
					RequestID: "rej", Model: "m",
					StatusCode: 429, ErrorType: domain.Err429, CreatedAt: time.Now(),
				})
			}
		}(g)
	}
	wg.Wait()

	require.NoError(t, f.Close(context.Background()))
	require.NoError(t, ew.Close(context.Background()))

	writer.mu.Lock()
	calls := len(writer.calls)
	var rows int
	for _, c := range writer.calls {
		rows += len(c.logs)
	}
	writer.mu.Unlock()
	require.Equal(t, billed, rows, "计费 flusher 独立排空全部 billed 明细")
	require.Equal(t, 1, calls, "同用户聚合一笔事务（flusher 语义不受 errlog 并行影响）")
	require.Equal(t, rejected, errs.count(), "errlog worker 独立排空全部拒绝行（与 flusher 互不串路）")
	require.Zero(t, ew.Queued(), "errlog 队列排空")
}
