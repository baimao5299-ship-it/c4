// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。

// SyncWorkerStats 价格同步 worker 状态（存活/状态最小集）。
type SyncWorkerStats struct {
	Running        bool  `json:"running"`           // 循环存活（Start 置位、cronLoop 退出复位——与 authSync/notify 一致）
	LastSyncUnixMs int64 `json:"last_sync_unix_ms"` // 最近一次 fetch 尝试时刻（fetch 失败也算尝试；0 = 尚未尝试）
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (w *SyncWorker) Stats() any {
	return SyncWorkerStats{
		Running:        w.running.Load(),
		LastSyncUnixMs: w.lastSync.Load(),
	}
}
