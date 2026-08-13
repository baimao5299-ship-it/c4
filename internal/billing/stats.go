// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。
// 采集纪律：原子读既有计数器（零锁零分配，O(1)）；不新增热路径埋点。

// FlusherStats billing flusher 状态（原子读组装，不锁模块内部）。
type FlusherStats struct {
	PendingLogs      int64 `json:"pending_logs"`       // 尚未落库的计费日志条数
	PendingWaterline int64 `json:"pending_waterline"`  // 水线（包级 var 直读）
	Warned           bool  `json:"warned"`             // 水线告警边沿是否置位
	LastFlushUnixMs  int64 `json:"last_flush_unix_ms"` // 最近一次成功落库时刻（0 = 尚未成功落库；空 flush/全失败不推进，G2-4）
}

// Stats 满足 server.StatsProvider（独立于 worker.Worker 契约）。
func (f *Flusher) Stats() any {
	return FlusherStats{
		PendingLogs:      f.pendingN.Load(),
		PendingWaterline: pendingWaterline,
		Warned:           f.warned.Load(),
		LastFlushUnixMs:  f.lastFlush.Load(),
	}
}
