package pricing

// /ops/workers pricing-sync Stats 与真实状态一致性单测（存活 + 最近同步时刻；
// typed struct 断言）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSyncWorkerStats(t *testing.T) {
	w := newTestWorker(&fakeFetcher{result: &FetchResult{}}, &fakeUpserter{}, &fakeSettings{url: "http://upstream/pricing"})
	st := w.Stats().(SyncWorkerStats)
	require.False(t, st.Running)
	require.Zero(t, st.LastSyncUnixMs, "未同步 = 0")

	require.NoError(t, w.Sync(context.Background()))
	st = w.Stats().(SyncWorkerStats)
	require.Greater(t, st.LastSyncUnixMs, int64(0), "Sync 完成时刻已记")
	require.GreaterOrEqual(t, st.LastSyncUnixMs, time.Now().Add(-time.Minute).UnixMilli())

	// fetch 失败也算一次同步尝试（状态可观，不静默）。
	w2 := newTestWorker(&fakeFetcher{err: errors.New("boom")}, &fakeUpserter{}, &fakeSettings{url: "http://upstream/pricing"})
	require.Error(t, w2.Sync(context.Background()))
	require.Greater(t, w2.Stats().(SyncWorkerStats).LastSyncUnixMs, int64(0), "失败尝试也记录最近同步时刻")
}
