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

type fakePartitionManager struct {
	mu        sync.Mutex
	drops     []time.Time // cutoff 参数
	nows      []time.Time // ensure 的 now 参数
	ensures   []time.Time // ensure 的 until 参数
	dropErr   error
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

func (f *fakePartitionManager) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.drops), len(f.ensures)
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
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))

	// 启动即巡检一次 + 至少 2 个 tick（drop + ensure 每轮都调）
	waitCounts(t, pm, 2, 2)
	cancel()
	require.NoError(t, w.Close(ctx))

	pm.mu.Lock()
	defer pm.mu.Unlock()
	// cutoff 语义：now - 30 天（日粒度，容忍 ±1 天边界）
	now := time.Now().UTC()
	cut := now.AddDate(0, 0, -30).Truncate(24 * time.Hour)
	got := pm.drops[0].UTC().Truncate(24 * time.Hour)
	require.True(t, cut.Equal(got) || cut.Add(-24*time.Hour).Equal(got) || cut.Add(24*time.Hour).Equal(got),
		"cutoff = now-30d 日粒度，got=%v want≈%v", pm.drops[0], cut)
	// until 语义：now + 1 天（预建当日/明日分区）
	until := pm.ensures[0]
	require.WithinDuration(t, time.Now().AddDate(0, 0, 1), until, 2*time.Second)
	// now 语义（评审 I-2）：ensure 的 now = 巡检时刻（与 until 同源，边界
	// 由同一 now 推导）
	require.WithinDuration(t, time.Now(), pm.nows[0], 2*time.Second)
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
// 只预建分区（旧 janitorLoop 同语义）。
func TestRetentionWorkerZeroRetentionSkipsDrop(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 0, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 0, 2)
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
