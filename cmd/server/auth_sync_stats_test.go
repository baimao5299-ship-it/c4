// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

// /ops/workers auth-sync Stats 与真实状态一致性单测（存活 + 最近 Reload 时刻；
// typed struct 断言）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthSyncStats(t *testing.T) {
	w := newAuthSync(&recAuthN{}, 5*time.Millisecond, nil)
	st := w.Stats().(authSyncStats)
	require.False(t, st.Running, "未 Start 不存活")
	require.Zero(t, st.LastReloadUnixMs, "未触发 Reload = 0")

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	require.Eventually(t, func() bool {
		st := w.Stats().(authSyncStats)
		return st.Running && st.LastReloadUnixMs > 0
	}, time.Second, 5*time.Millisecond, "周期循环存活且 Reload 时刻已记")

	cancel()
	time.Sleep(50 * time.Millisecond) // 等循环退出（select 上 ctx.Done 立即返回）
	require.False(t, w.Stats().(authSyncStats).Running, "循环退出后复位")
}
