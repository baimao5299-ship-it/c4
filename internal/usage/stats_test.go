// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

// /ops/workers 各 worker Stats 与真实状态一致性单测（spec 2026-08-11 验收：
// pending 增长、丢弃计数、Status 同步；typed struct 断言）。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// --- Recorder ---

func TestRecorderStats(t *testing.T) {
	r := New(UsageConfig{BatchSize: 10, FlushInterval: time.Hour, StatsFlushInterval: time.Hour},
		&memLogStore{}, &memStatStore{}, nil)
	base := time.Now().UTC().Truncate(time.Hour)

	// 同桶 3 条 + 异桶 2 条：pending 累计 5，桶数 2（同桶合并）。
	rec := func(uid int64, rid string) {
		r.Record(&domain.UsageLog{RequestID: rid, UserID: uid, Model: "m",
			Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: base})
	}
	rec(1, "a")
	rec(1, "b")
	rec(1, "c")
	rec(2, "d")
	rec(2, "e")

	st := r.Stats().(RecorderStats) // typed struct 断言
	require.Equal(t, int64(5), st.PendingLogs, "pending 与 Record 累计一致")
	require.Equal(t, int64(2), st.StatBuckets, "同桶合并后累计创建桶数 = 2")
	require.Equal(t, pendingWaterline, st.PendingWaterline, "水线包级 var 直读")
	require.False(t, st.Warned)
	require.Equal(t, 5, r.Pending(), "既有 accessor 同步一致")

	// Aggregate 路径（billed 统计面）：只进桶不进 pending。
	r.Aggregate(&domain.UsageLog{RequestID: "f", UserID: 3, Model: "m",
		Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, CreatedAt: base})
	st = r.Stats().(RecorderStats)
	require.Equal(t, int64(5), st.PendingLogs, "Aggregate 不进 pending")
	require.Equal(t, int64(3), st.StatBuckets, "Aggregate 新桶也计入累计")
}

// --- ErrLogWorker ---

func TestErrLogWorkerStats(t *testing.T) {
	w := NewErrLogWorker(ErrLogConfig{QueueSize: 2, ExemptQueueSize: 1, BatchSize: 10, FlushInterval: time.Hour},
		&captureErrLogInserter{}, nil)
	l := func(rid string) *domain.UsageLog {
		return &domain.UsageLog{RequestID: rid, Model: "m", Format: domain.FormatOpenAIChat,
			StatusCode: 429, ErrorType: domain.Err429, CreatedAt: time.Now()}
	}

	w.EnqueueRejected(l("r1"))
	w.EnqueueRejected(l("r2"))
	w.EnqueueRejected(l("r3")) // 队列满 → 采样丢弃

	st := w.Stats().(ErrLogWorkerStats)
	require.Equal(t, 2, st.Queued, "队列占用 = 实际积压")
	require.Equal(t, 2, st.QueueCap)
	require.Equal(t, 1, st.ExemptQueueCap)
	require.Equal(t, int64(1), st.DroppedReject, "拒绝行丢弃计数")
	require.Zero(t, st.DroppedExempt, "双轨行未丢弃")
	require.Zero(t, st.Inserted)
	require.Equal(t, 2, w.Queued(), "既有 accessor 同步一致")

	// 双轨行进豁免队列；flush 落盘 → inserted 与真实落盘一致。
	w.EnqueueError(l("e1"))
	st = w.Stats().(ErrLogWorkerStats)
	require.Equal(t, 3, st.Queued)
	w.flush()
	st = w.Stats().(ErrLogWorkerStats)
	require.Equal(t, int64(3), st.Inserted, "inserted = 真实落盘行数（豁免优先整批排空）")
	require.Zero(t, st.Queued, "flush 后排空")
	require.Equal(t, int64(3), w.Inserted(), "既有 accessor 同步一致")
}

// --- Retention ---

// countingPartitionManager 可配置 DROP 返回值的 PartitionManager（观测面断言：
// runOnce 缓存真实返回值）。
type countingPartitionManager struct {
	mu                 sync.Mutex
	logDrops, errDrops int
	statsDrops         int
	dropErr            error // usage_logs drop 失败注入
}

func (c *countingPartitionManager) DropUsageLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.logDrops, c.dropErr
}
func (c *countingPartitionManager) DropErrLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errDrops, nil
}
func (c *countingPartitionManager) DropUsageStatsPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statsDrops, nil
}
func (c *countingPartitionManager) EnsureUsageLogPartitions(ctx context.Context, now, until time.Time) error {
	return nil
}
func (c *countingPartitionManager) EnsureErrLogPartitions(ctx context.Context, now, until time.Time) error {
	return nil
}
func (c *countingPartitionManager) EnsureUsageStatsPartitions(ctx context.Context, now, until time.Time) error {
	return nil
}

func TestRetentionStats(t *testing.T) {
	pm := &countingPartitionManager{logDrops: 3, errDrops: 1, statsDrops: 0}
	w := NewRetention(RetentionConfig{
		LogRetentionDays: 2, ErrLogRetentionDays: 2, StatsRetentionDays: 2,
	}, pm, nil)
	require.Zero(t, w.Stats().(RetentionWorkerStats).LastPatrolUnixMs, "未巡检 = 0")

	w.runOnce()
	st := w.Stats().(RetentionWorkerStats)
	require.Greater(t, st.LastPatrolUnixMs, int64(0), "巡检完成时刻已记")
	require.Equal(t, int64(3), st.LastDroppedLogPartitions, "DROP 返回值缓存 = 真实分区数")
	require.Equal(t, int64(1), st.LastDroppedErrLogPartitions)
	require.Zero(t, st.LastDroppedStatsPartitions)
	require.Equal(t, 2, st.LogRetentionDays)

	// 失败不覆盖上一轮值（保留观测连续性），LastPatrol 仍推进。
	pm.dropErr = errors.New("boom")
	w.runOnce()
	st = w.Stats().(RetentionWorkerStats)
	require.Equal(t, int64(3), st.LastDroppedLogPartitions, "失败轮保留上一轮 DROP 计数")
	require.Greater(t, st.LastPatrolUnixMs, int64(0))
}
