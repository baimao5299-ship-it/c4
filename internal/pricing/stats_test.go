// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

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

// TestSyncWorkerStatsRunningReset Running 语义：Start 置位、循环退出复位
// （与 authSync/notify 一致——startOnce 是幂等守卫，非存活语义）。
func TestSyncWorkerStatsRunningReset(t *testing.T) {
	w := newTestWorker(&fakeFetcher{result: &FetchResult{}}, &fakeUpserter{}, &fakeSettings{url: "http://upstream/pricing"})
	w.wait = waitBlock // cronLoop 保持存活直至 ctx 取消
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, w.Start(ctx))
	require.Eventually(t, func() bool { return w.Stats().(SyncWorkerStats).Running },
		time.Second, time.Millisecond, "Start 后循环存活")
	cancel()
	require.Eventually(t, func() bool { return !w.Stats().(SyncWorkerStats).Running },
		time.Second, time.Millisecond, "循环退出后复位")
}
