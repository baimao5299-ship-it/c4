package usage

// retention worker 调度测试：短 ticker + fake PartitionManager 记录调用参数。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var errBoom = errors.New("boom")

// fakePartitionManager 三表计数 fake（usage_logs / err_logs / usage_stats 各自
// 独立计数——三表独立调度断言：cutoff 独立、失败隔离）。
type fakePartitionManager struct {
	mu        sync.Mutex
	drops     []time.Time // usage_logs cutoff 参数
	nows      []time.Time // usage_logs ensure 的 now 参数
	ensures   []time.Time // usage_logs ensure 的 until 参数
	edrops    []time.Time // err_logs cutoff 参数
	enows     []time.Time // err_logs ensure 的 now 参数
	eensures  []time.Time // err_logs ensure 的 until 参数
	sdrops    []time.Time // usage_stats cutoff 参数
	snows     []time.Time // usage_stats ensure 的 now 参数
	sensures  []time.Time // usage_stats ensure 的 until 参数
	dropErr   error       // usage_logs drop 失败注入
	edropErr  error       // err_logs drop 失败注入（失败隔离断言）
	sdropErr  error       // usage_stats drop 失败注入（失败隔离断言）
	ensureErr error
}

func (f *fakePartitionManager) DropUsageLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drops = append(f.drops, cutoff)
	return 0, f.dropErr
}

func (f *fakePartitionManager) EnsureUsageLogPartitions(ctx context.Context, now, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nows = append(f.nows, now)
	f.ensures = append(f.ensures, until)
	return f.ensureErr
}

func (f *fakePartitionManager) DropErrLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edrops = append(f.edrops, cutoff)
	return 0, f.edropErr
}

func (f *fakePartitionManager) EnsureErrLogPartitions(ctx context.Context, now, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enows = append(f.enows, now)
	f.eensures = append(f.eensures, until)
	return f.ensureErr
}

func (f *fakePartitionManager) DropUsageStatsPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sdrops = append(f.sdrops, cutoff)
	return 0, f.sdropErr
}

func (f *fakePartitionManager) EnsureUsageStatsPartitions(ctx context.Context, now, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snows = append(f.snows, now)
	f.sensures = append(f.sensures, until)
	return f.ensureErr
}

func (f *fakePartitionManager) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.drops), len(f.ensures)
}

func (f *fakePartitionManager) errCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.edrops), len(f.eensures)
}

func (f *fakePartitionManager) statsCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sdrops), len(f.sensures)
}

// waitCounts 轮询直到 drops/ensures 达到目标（短 ticker 调度断言）。
func waitCounts(t *testing.T, f *fakePartitionManager, wantDrops, wantEnsures int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, e := f.counts()
		if d >= wantDrops && e >= wantEnsures {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	d, e := f.counts()
	require.Failf(t, "worker did not tick in time", "drops=%d ensures=%d (want %d/%d)", d, e, wantDrops, wantEnsures)
}

func TestRetentionWorkerTicks(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, ErrLogRetentionDays: 7, StatsRetentionDays: 180, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))

	// 启动即巡检一次 + 至少 2 个 tick（drop + ensure 每轮都调——两表各自）
	waitCounts(t, pm, 2, 2)
	cancel()
	require.NoError(t, w.Close(ctx))

	pm.mu.Lock()
	defer pm.mu.Unlock()
	// cutoff 语义：usage_logs now - 30 天（日粒度，容忍 ±1 天边界）
	now := time.Now().UTC()
	cut := now.AddDate(0, 0, -30).Truncate(24 * time.Hour)
	got := pm.drops[0].UTC().Truncate(24 * time.Hour)
	require.True(t, cut.Equal(got) || cut.Add(-24*time.Hour).Equal(got) || cut.Add(24*time.Hour).Equal(got),
		"cutoff = now-30d 日粒度，got=%v want≈%v", pm.drops[0], cut)
	// err_logs cutoff 独立：now - 7 天（ErrLogRetentionDays 独立保留期）
	ecut := now.AddDate(0, 0, -7).Truncate(24 * time.Hour)
	egot := pm.edrops[0].UTC().Truncate(24 * time.Hour)
	require.True(t, ecut.Equal(egot) || ecut.Add(-24*time.Hour).Equal(egot) || ecut.Add(24*time.Hour).Equal(egot),
		"err_logs cutoff = now-7d 独立保留期，got=%v want≈%v", pm.edrops[0], ecut)
	require.Len(t, pm.edrops, len(pm.drops), "三表每轮各自 DROP（同一调度循环）")
	// usage_stats cutoff 独立：now - 180 天（StatsRetentionDays 独立长保留）
	scut := now.AddDate(0, 0, -180).Truncate(24 * time.Hour)
	sgot := pm.sdrops[0].UTC().Truncate(24 * time.Hour)
	require.True(t, scut.Equal(sgot) || scut.Add(-24*time.Hour).Equal(sgot) || scut.Add(24*time.Hour).Equal(sgot),
		"usage_stats cutoff = now-180d 独立长保留期，got=%v want≈%v", pm.sdrops[0], scut)
	// until 语义：now + 1 天（预建当日/明日分区）
	until := pm.ensures[0]
	require.WithinDuration(t, time.Now().AddDate(0, 0, 1), until, 2*time.Second)
	// now 语义（评审 I-2）：ensure 的 now = 巡检时刻（与 until 同源，边界
	// 由同一 now 推导）
	require.WithinDuration(t, time.Now(), pm.nows[0], 2*time.Second)
	require.Len(t, pm.eensures, len(pm.ensures), "三表各自预建分区")
	require.Len(t, pm.sensures, len(pm.ensures), "usage_stats 独立预建分区")
	require.WithinDuration(t, time.Now(), pm.snows[0], 2*time.Second)
}

// TestRetentionWorkerStartsWithImmediateRun 启动即巡检（不等到第一个 tick）。
func TestRetentionWorkerStartsWithImmediateRun(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 7, TickerInterval: time.Hour}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 1, 1) // 不等 ticker（1h）即完成一轮
	cancel()
	require.NoError(t, w.Close(ctx))
}

// TestRetentionWorkerZeroRetention SkipsDrop LogRetentionDays<=0 → 不删除，
// 只预建分区（旧 janitorLoop 同语义）；三表独立：usage_logs 0 天不删除、
// err_logs 30 天删除、usage_stats 未配置（0）不删除。
func TestRetentionWorkerZeroRetentionSkipsDrop(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 0, ErrLogRetentionDays: 30, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 0, 2)
	cancel()
	require.NoError(t, w.Close(ctx))
	pm.mu.Lock()
	defer pm.mu.Unlock()
	require.Len(t, pm.drops, 0, "usage_logs 保留天数 0 → 不删除")
	require.NotEmpty(t, pm.edrops, "err_logs 保留天数 30 → 独立删除")
	require.NotEmpty(t, pm.eensures, "err_logs 预建分区")
	require.Empty(t, pm.sdrops, "usage_stats 未配置保留天数 → 不删除")
	require.NotEmpty(t, pm.sensures, "usage_stats 仍预建分区（保留期仅管删除）")
}

// TestRetentionWorkerStatsFailureIsolated C32 扩展：usage_stats DROP 失败不影响
// 明细两表（三表逐表错误隔离——180 天清理失败不连带 30 天/7 天清理）。
func TestRetentionWorkerStatsFailureIsolated(t *testing.T) {
	pm := &fakePartitionManager{sdropErr: errBoom}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, ErrLogRetentionDays: 7, StatsRetentionDays: 180, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 2, 2)
	sd, se := pm.statsCounts()
	require.GreaterOrEqual(t, sd, 2, "usage_stats drop 失败仍每轮重试")
	require.GreaterOrEqual(t, se, 2, "usage_stats ensure 不受 drop 失败影响")
	ed, ee := pm.errCounts()
	require.GreaterOrEqual(t, ed, 2, "err_logs 不受 usage_stats 失败影响")
	require.GreaterOrEqual(t, ee, 2)
	cancel()
	require.NoError(t, w.Close(ctx))
}

// TestRetentionWorkerErrLogsFailureIsolated C32：一表 DROP 失败不影响另一表
//（err_logs drop 失败 → usage_logs 仍正常 drop/ensure，下轮重试各自独立）。
func TestRetentionWorkerErrLogsFailureIsolated(t *testing.T) {
	pm := &fakePartitionManager{edropErr: errBoom}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, ErrLogRetentionDays: 7, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 2, 2)
	ed, ee := pm.errCounts()
	require.GreaterOrEqual(t, ed, 2, "err_logs drop 失败仍每轮重试")
	require.GreaterOrEqual(t, ee, 2, "err_logs ensure 不受 drop 失败影响")
	cancel()
	require.NoError(t, w.Close(ctx))
}

// TestRetentionWorkerErrorTolerated 单轮失败不中断循环（Warn + 下轮重试）。
func TestRetentionWorkerErrorTolerated(t *testing.T) {
	pm := &fakePartitionManager{dropErr: errBoom, ensureErr: errBoom}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 2, 2) // 失败后仍继续 tick
	cancel()
	require.NoError(t, w.Close(ctx))
}

// TestRetentionWorkerCloseIdempotent Close 未 Start 也安全 + 重复 Close 幂等
//（worker.Worker 契约）。
func TestRetentionWorkerCloseIdempotent(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30}, pm, nil)
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.Background()))
}

// TestRetentionWorkerStartTwiceFails 重复 Start 报错（Recorder 同契约）。
func TestRetentionWorkerStartTwiceFails(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, w.Start(ctx))
	require.Error(t, w.Start(ctx))
}
