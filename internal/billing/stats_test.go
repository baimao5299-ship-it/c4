// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// /ops/workers billing flusher Stats 与真实状态一致性单测（pending 增长 +
// lastFlush 时间戳；typed struct 断言）。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestFlusherStats(t *testing.T) {
	f := newTestFlusher(&fakeDeductWriter{})
	require.Zero(t, f.Stats().(FlusherStats).LastFlushUnixMs, "尚未 flush = 0")

	f.Record(&domain.UsageLog{UserID: 1, Model: "m", Format: domain.FormatOpenAIChat,
		StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 10, Cost: 5, CreatedAt: time.Now()})
	f.Record(&domain.UsageLog{UserID: 1, Model: "m", Format: domain.FormatOpenAIChat,
		StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 20, Cost: 7, CreatedAt: time.Now()})
	st := f.Stats().(FlusherStats)
	require.Equal(t, int64(2), st.PendingLogs, "pending 与 Record 累计一致")
	require.Equal(t, pendingWaterline, st.PendingWaterline)
	require.False(t, st.Warned)

	f.flush()
	st = f.Stats().(FlusherStats)
	require.Zero(t, st.PendingLogs, "flush 后排空")
	require.Greater(t, st.LastFlushUnixMs, int64(0), "lastFlush = 最近实际 flush 时刻")

	// 空 pending 的 flush 不推进 lastFlush（观测"最近何时真正落过库"）。
	prev := st.LastFlushUnixMs
	f.flush()
	require.Equal(t, prev, f.Stats().(FlusherStats).LastFlushUnixMs, "空 flush 不更新 lastFlush")
}
