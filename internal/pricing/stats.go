package pricing

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。

// SyncWorkerStats 价格同步 worker 状态（存活/状态最小集）。
type SyncWorkerStats struct {
	Running        bool  `json:"running"`           // Start 后循环存活
	LastSyncUnixMs int64 `json:"last_sync_unix_ms"` // 最近一次同步尝试完成时刻（0 = 尚未同步）
}

// Stats 满足 server.StatsProvider（独立于 worker.Worker 契约）。
func (w *SyncWorker) Stats() any {
	return SyncWorkerStats{
		Running:        w.startOnce.Load(),
		LastSyncUnixMs: w.lastSync.Load(),
	}
}
