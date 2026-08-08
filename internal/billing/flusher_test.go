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

func newTestFlusher(writer *fakeDeductWriter) *Flusher {
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

// TestFlusherGroupsByUser 聚合分组：按 userID 归并 cost + 日志，Close 排空 +
// 全量 flush 后每用户一笔 DeductAndLog 事务。
func TestFlusherGroupsByUser(t *testing.T) {
	writer := &fakeDeductWriter{}
	f := newTestFlusher(writer)
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

// TestFlusherSaturationBlocks 有界 channel 饱和阻塞反压（不得丢数据）：
// cap 16384 满后 Record 阻塞，Close 排空后解除。
func TestFlusherSaturationBlocks(t *testing.T) {
	writer := &fakeDeductWriter{}
	f := newTestFlusher(writer) // 未 Start：无消费方

	filled := make(chan struct{})
	go func() {
		for i := 0; i < 16384; i++ {
			f.Record(&domain.UsageLog{UserID: 1, Cost: 1})
		}
		close(filled)
	}()
	select {
	case <-filled:
	case <-time.After(5 * time.Second):
		t.Fatal("16384 条应可全部入队（channel cap）")
	}
	blocked := make(chan struct{})
	go func() {
		f.Record(&domain.UsageLog{UserID: 2, Cost: 1})
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("饱和后 Record 必须阻塞反压（不得丢数据）")
	case <-time.After(200 * time.Millisecond):
	}
	require.NoError(t, f.Close(context.Background()))
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 排空后 Record 应解除阻塞")
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 2, "按 user 两笔")
	byUID := map[int64]deductCall{}
	for _, c := range writer.calls {
		byUID[c.userID] = c
	}
	require.Equal(t, int64(16384), byUID[1].cost, "user1 聚合 16384")
	require.Equal(t, int64(1), byUID[2].cost, "user2 阻塞解除后入队")
}
