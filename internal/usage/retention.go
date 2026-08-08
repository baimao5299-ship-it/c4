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
// DROP 过期分区 + 预建未来分区，不感知分区表内部 DDL。
type PartitionManager interface {
	EnsureUsageLogPartitions(ctx context.Context, until time.Time) error
	DropUsageLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// RetentionConfig retention worker 配置。
type RetentionConfig struct {
	LogRetentionDays int           // 分区保留天数（config usage.log_retention_days；<= 0 = 不删除）
	TickerInterval   time.Duration // 巡检周期（生产 1h；测试注入短周期；<= 0 兜底 1h）
}

// RetentionWorker 按日分区保留 worker（worker.Worker 契约，Name="retention"）：
// 每小时巡检一次——
//   - DROP 分区下界 < now - LogRetentionDays 的分区（DROP TABLE O(1)，比
//     逐行 DELETE 快 5~6 个量级；按分区名日期判定，无需查元数据）
//   - 预建 当日 + 未来 1 天 分区（PG 无自动建分区，防日界跨区插入失败）
//
// 与 Recorder 解耦（不依赖 rec.logCh）：DROP 幂等，无排空需求，Close 直接
// 返回 nil。
type RetentionWorker struct {
	cfg     RetentionConfig
	parts   PartitionManager
	log     *logx.Logger
	started atomic.Bool
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

// runOnce 单轮巡检：DROP 过期分区 + 预建未来分区；失败 Warn 不中断循环
// （下一轮重试）。
func (w *RetentionWorker) runOnce() {
	now := time.Now()
	if w.cfg.LogRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -w.cfg.LogRetentionDays)
		n, err := w.parts.DropUsageLogPartitionsBefore(context.Background(), cutoff)
		if err != nil {
			if w.log != nil {
				w.log.Warn("retention drop partitions failed", logx.Error(err))
			}
		} else if n > 0 && w.log != nil {
			w.log.Info("retention dropped partitions", logx.Int("count", n))
		}
	}
	if err := w.parts.EnsureUsageLogPartitions(context.Background(), now.AddDate(0, 0, 1)); err != nil {
		if w.log != nil {
			w.log.Warn("retention pre-create partitions failed", logx.Error(err))
		}
	}
}

// Close 幂等（worker.Worker 契约）：DROP/预建均幂等，无排空需求。
func (w *RetentionWorker) Close(ctx context.Context) error { return nil }
