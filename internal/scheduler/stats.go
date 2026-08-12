// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。
// 采集纪律：len(channel) 零成本；快照状态走 snapshot registry Status，不重复。

// SchedulerStats 调度器状态（异步状态回写队列占用；快照/路由状态经注册表
// Status 直出，此处不重复采集）。
type SchedulerStats struct {
	PendingWritebacks int `json:"pending_writebacks"` // 待回写 DB 的状态写队列积压
	WritebackCap      int `json:"writeback_cap"`      // 队列容量（满 → 丢弃 DB 回写，内存已生效）
}

// Stats 满足 server.StatsProvider（独立于 worker.Worker 契约）。
func (s *Scheduler) Stats() any {
	return SchedulerStats{
		PendingWritebacks: len(s.writeCh),
		WritebackCap:      cap(s.writeCh),
	}
}
