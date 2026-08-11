// 归属说明：retention worker 放 usage 包——原 Recorder.janitorLoop（逐行
// DELETE）已删除，保留策略整体移交本 worker；保留天数与 Recorder 配置同源
// （config usage.log_retention_days），故与 Recorder 同包管理。
package usage

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go-proxy-mini/pkg/logx"
)

// PartitionManager 分区管理面（repository.Repository 实现）：保留策略只需
// DROP 过期分区 + 预建未来分区，不感知分区表内部 DDL。now/until 由调用方
// 传入（start 边界由 now 推导，测试可注入时钟）。三表各自独立调度（保留期
// 独立：LogRetentionDays / ErrLogRetentionDays / StatsRetentionDays）。
type PartitionManager interface {
	EnsureUsageLogPartitions(ctx context.Context, now, until time.Time) error
	DropUsageLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error)
	EnsureErrLogPartitions(ctx context.Context, now, until time.Time) error
	DropErrLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error)
	EnsureUsageStatsPartitions(ctx context.Context, now, until time.Time) error
	DropUsageStatsPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// RetentionConfig retention worker 配置。
type RetentionConfig struct {
	LogRetentionDays    int           // usage_logs 分区保留天数（config usage.log_retention_days；<= 0 = 不删除）
	ErrLogRetentionDays int           // err_logs 分区保留天数（config usage.errlog_retention_days，默认 7 天短保留——错误审计；<= 0 = 不删除）
	StatsRetentionDays  int           // usage_stats 分区保留天数（config usage.stats_retention_days，默认 180 天——聚合统计长保留；<= 0 = 不删除）
	TickerInterval      time.Duration // 巡检周期（生产 1h；测试注入短周期；<= 0 兜底 1h）
}

// RetentionWorker 按日分区保留 worker（worker.Worker 契约，Name="retention"）：
// 每小时巡检一次——三表各自独立调度（usage_logs 按 LogRetentionDays、err_logs
// 按 ErrLogRetentionDays、usage_stats 按 StatsRetentionDays，同一循环无新增
// goroutine）：
//   - DROP 分区下界 < now - 保留天数的分区（DROP TABLE O(1)，比逐行 DELETE
//     快 5~6 个量级；按分区名日期判定，无需查元数据——usage_stats 保留清理
//     用户裁决 2026-08-11：PG DELETE 不释放空间，必须分区 DROP）
//   - 预建 当日 + 未来 1 天 分区（PG 无自动建分区，防日界跨区插入失败）
//
// DROP × 在途插入竞态（评审 I-3）：DROP TABLE 需 ACCESS EXCLUSIVE 锁，与
// 在途插入事务串行；能落进被 DROP 分区（保留期前）的行只有回放/陈旧
// created_at 的延迟日志——该分区数据本就在保留语义内（要清理）。万一插入
// 恰好失败 → 走落库失败路径（Warn + 丢弃，与普通批量落库失败同语义，不自愈
// 不重试），可接受。
//
// 与 Recorder/ErrLogWorker 解耦（不依赖明细管道）：DROP 幂等，无排空需求，
// Close 直接返回 nil。
type RetentionWorker struct {
	cfg     RetentionConfig
	parts   PartitionManager
	log     *logx.Logger
	started atomic.Bool
	// 观测面（/ops/workers；runOnce 收尾原子写，零新增 DB）：lastPatrol 最近
	// 一次巡检完成时刻（UnixMilli；0 = 尚未巡检）；lastDrop* 最近一轮各表
	// DROP 分区数（缓存 runOnce 现有返回值——DROP 的 n 即真实 DB 答案）。
	lastPatrol      atomic.Int64
	lastDropLogs    atomic.Int64
	lastDropErrLogs atomic.Int64
	lastDropStats   atomic.Int64
}

func NewRetention(cfg RetentionConfig, parts PartitionManager, log *logx.Logger) *RetentionWorker {
	return &RetentionWorker{cfg: cfg, parts: parts, log: log}
}

// Name worker.Worker 契约（注册顺序无依赖——DROP/预建均幂等）。
func (w *RetentionWorker) Name() string { return "retention" }

func (w *RetentionWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("retention: already started")
	}
	go w.loop(ctx)
	return nil
}

func (w *RetentionWorker) loop(ctx context.Context) {
	interval := w.cfg.TickerInterval
	if interval <= 0 {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	w.runOnce() // 启动即巡检（兜底 bootstrap 预建 + 清理历史遗留分区）
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.runOnce()
		}
	}
}

// runOnce 单轮巡检：三表各自 DROP 过期分区（独立 cutoff）+ 预建未来分区；失败
// Warn 不中断循环（下一轮重试）。now 现取一次，cutoff/ensure 边界共用同一时钟
// （评审 I-2：边界由调用方 now 推导，不各取各的）。逐表错误隔离（一表失败
// 不影响他表——C32 纪律：usage_stats 180 天 DROP 失败不连带明细表清理）。
func (w *RetentionWorker) runOnce() {
	now := time.Now()
	ctx := context.Background()
	// 各表 DROP 计数收尾原子写（观测面缓存 runOnce 现有返回值；失败不覆盖——
	// 保留上一轮值，LastPatrol 仍推进标记"巡检发生过"）。
	if w.cfg.LogRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -w.cfg.LogRetentionDays)
		n, err := w.parts.DropUsageLogPartitionsBefore(ctx, cutoff)
		if err != nil {
			if w.log != nil {
				w.log.Warn("retention drop usage_logs partitions failed", logx.Error(err))
			}
		} else {
			w.lastDropLogs.Store(int64(n))
			if n > 0 && w.log != nil {
				w.log.Info("retention dropped usage_logs partitions", logx.Int("count", n))
			}
		}
	}
	if w.cfg.ErrLogRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -w.cfg.ErrLogRetentionDays)
		n, err := w.parts.DropErrLogPartitionsBefore(ctx, cutoff)
		if err != nil {
			if w.log != nil {
				w.log.Warn("retention drop err_logs partitions failed", logx.Error(err))
			}
		} else {
			w.lastDropErrLogs.Store(int64(n))
			if n > 0 && w.log != nil {
				w.log.Info("retention dropped err_logs partitions", logx.Int("count", n))
			}
		}
	}
	if w.cfg.StatsRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -w.cfg.StatsRetentionDays)
		n, err := w.parts.DropUsageStatsPartitionsBefore(ctx, cutoff)
		if err != nil {
			if w.log != nil {
				w.log.Warn("retention drop usage_stats partitions failed", logx.Error(err))
			}
		} else {
			w.lastDropStats.Store(int64(n))
			if n > 0 && w.log != nil {
				w.log.Info("retention dropped usage_stats partitions", logx.Int("count", n))
			}
		}
	}
	if err := w.parts.EnsureUsageLogPartitions(ctx, now, now.AddDate(0, 0, 1)); err != nil {
		if w.log != nil {
			w.log.Warn("retention pre-create usage_logs partitions failed", logx.Error(err))
		}
	}
	if err := w.parts.EnsureErrLogPartitions(ctx, now, now.AddDate(0, 0, 1)); err != nil {
		if w.log != nil {
			w.log.Warn("retention pre-create err_logs partitions failed", logx.Error(err))
		}
	}
	if err := w.parts.EnsureUsageStatsPartitions(ctx, now, now.AddDate(0, 0, 1)); err != nil {
		if w.log != nil {
			w.log.Warn("retention pre-create usage_stats partitions failed", logx.Error(err))
		}
	}
	w.lastPatrol.Store(now.UnixMilli())
}

// Close 幂等（worker.Worker 契约）：DROP/预建均幂等，无排空需求。
func (w *RetentionWorker) Close(ctx context.Context) error { return nil }
