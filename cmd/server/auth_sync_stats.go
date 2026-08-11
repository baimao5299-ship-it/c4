package main

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。

// authSyncStats auth-sync 周期刷新 worker 状态（存活/状态最小集）。
type authSyncStats struct {
	Running          bool  `json:"running"`             // 周期循环存活（Start 置位、退出复位）
	LastReloadUnixMs int64 `json:"last_reload_unix_ms"` // 最近一次 Reload 完成时刻（0 = 尚未触发）
}

// Stats 满足 server.StatsProvider（独立于 worker.Worker 契约）。
func (w *authSync) Stats() any {
	return authSyncStats{
		Running:          w.running.Load(),
		LastReloadUnixMs: w.lastReload.Load(),
	}
}
