package rule

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。
// 采集纪律：原子读 + len(channel)（零锁零分配，O(1)）。

// RuleEngineStats 规则引擎状态（队列占用 + 事件丢弃累计）。
type RuleEngineStats struct {
	Queued            int   `json:"queued"`              // 队列积压事件数
	QueueCap          int   `json:"queue_cap"`           // 事件队列容量（丢弃阈值）
	Dropped           int64 `json:"dropped"`             // 队列满丢弃累计（atomic.Uint64 转 int64——JSON 数字精度）
	DropWarnThreshold int64 `json:"drop_warn_threshold"` // 丢弃告警阈值（包级 var 直读）
}

// Stats 满足 server.StatsProvider（独立于 worker.Worker 契约）。
func (e *RuleEngine) Stats() any {
	return RuleEngineStats{
		Queued:            len(e.ch),
		QueueCap:          cap(e.ch),
		Dropped:           int64(e.dropped.Load()),
		DropWarnThreshold: ruleDropWarnThreshold,
	}
}
