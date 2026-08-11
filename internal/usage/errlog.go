package usage

// err_logs 错误明细落盘 worker（用户裁决分表设计：独立于计费 Flusher 与
// Recorder 的错误日志落盘通道——独立类型、独立队列、独立锁，互不阻塞）。
//
// 背压（风暴采样）：有界队列 + producer 非阻塞投递（队列满 → 丢弃 + 原子计数）
// ——拒绝风暴不阻塞请求热路径、不爆内存、不淹没 DB（DB 写速率由 batch/interval
// 配置有界 = BatchSize/FlushInterval，不随风暴放大；丢弃即采样式落盘的采样面）。
//
// 双队列按来源（provenance）分类（架构审查 B2，用户裁决）——不可按 error_type
// 推断（Err429/ErrBilling/ErrAuth 在拒绝类与双轨类同时出现）：
//   - 豁免队列（exemptQ）：**双轨行**（abort/failover 已计费错误：finish/
//     recordLog 投递的 usage_logs 错误行）——**不参与拒绝风暴采样丢弃，恒落盘**
//     （已计费错误审计价值最高；仅本队列自身溢出才丢——排空 1k/s 下正常不可达）。
//   - 普通队列（rejectQ）：**拒绝行**（recordRejected：401/429/402/400/404 本地
//     拒绝 + 组限流）——风暴采样丢弃面。
//
// 不变式（架构审查 A2 固化）：err_logs ⊇ usage_logs 全部错误行（双轨行永不
// 采样）∪ 采样后的拒绝行；丢弃计数（DroppedReject/DroppedExempt）即统计面
// usagestat（全量）与 err_logs（采样）的对账指标。停机：Close 幂等排空（受
// shutdown ctx 预算约束，与 flusher 停机语义一致）。

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

// ErrLogConfig err_logs 落盘 worker 节奏。
type ErrLogConfig struct {
	QueueSize       int           // 拒绝队列容量（背压面：满 → 丢弃；默认 4096 ≈ 1MB 量级内存）
	ExemptQueueSize int           // 豁免队列容量（双轨行；默认 1024——恒落盘语义，仅自身溢出才丢）
	BatchSize       int           // 每批落盘行数（默认 500；单条 CreateBulk 有界）
	FlushInterval   time.Duration // 批间隔（默认 500ms；DB 写速率上界 = BatchSize/FlushInterval）
}

// ErrLogInserter err_logs 批量插入面（repository.ErrLogRepo 实现）。
type ErrLogInserter interface {
	InsertBatch(ctx context.Context, logs []*domain.UsageLog) error
}

// errlogFlushTimeout 单批落盘超时（防慢 DB/挂死连接无界占用 worker 循环——
// 超时即失败丢弃，审计明细非计费，不无限重试）。
const errlogFlushTimeout = 5 * time.Second

// errlogDropWarnThreshold 拒绝行丢弃累计告警阈值：丢弃计数 ≥ 阈值后 Warn 恰好
// 一次（风暴采样丢弃的可观测面；队列排空后边沿回落——每风暴一次，不刷屏）。
// var（非 const）：测试注入小阈值。
var errlogDropWarnThreshold int64 = 10_000

// ErrLogWorker 错误明细落盘 worker（worker.Worker 契约，Name="errlog"）。
type ErrLogWorker struct {
	cfg    ErrLogConfig
	writer ErrLogInserter
	log    *logx.Logger
	// mu 保护 closed 标志与投递（Enqueue 短临界区：closed 检查 + 非阻塞 send——
	// 恒纳秒级，错误路径调用非热路径）。Close 置位 closed 后再排空——与投递
	// 互斥串行，无"排空尾窗口静默丢"（架构审查 S4）。
	mu     sync.Mutex
	closed bool
	// exemptQ 豁免队列（双轨行，恒落盘）；rejectQ 普通队列（拒绝行，风暴采样）。
	// 有界容量见 cfg；投递 select-default 非阻塞。
	exemptQ chan *domain.UsageLog
	rejectQ chan *domain.UsageLog
	// 丢弃计数按类拆分（架构审查 S3：reject 丢弃 / 双轨丢弃，对账指标）：
	// 双轨丢弃 >0 即告警（异常态——豁免队列溢出）。
	droppedReject atomic.Int64
	droppedExempt atomic.Int64
	inserted      atomic.Int64 // 成功落盘计数（观测/测试）
	warnReject    atomic.Bool  // 拒绝丢弃告警边沿（≥ 阈值告警恰好一次；队列排空回落）
	warnExempt    atomic.Bool  // 双轨丢弃告警边沿（>0 即告警一次；排空回落）
	started       atomic.Bool
	loopDone      chan struct{}
	closeOnce     sync.Once
}

func NewErrLogWorker(cfg ErrLogConfig, writer ErrLogInserter, log *logx.Logger) *ErrLogWorker {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4096
	}
	if cfg.ExemptQueueSize <= 0 {
		cfg.ExemptQueueSize = 1024
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 500 * time.Millisecond
	}
	return &ErrLogWorker{
		cfg: cfg, writer: writer, log: log,
		exemptQ:  make(chan *domain.UsageLog, cfg.ExemptQueueSize),
		rejectQ:  make(chan *domain.UsageLog, cfg.QueueSize),
		loopDone: make(chan struct{}),
	}
}

// Name worker.Worker 契约（wm 反向排空：errlog 在 rec 之后注册 → 先于 rec 排空
// 错误明细；与计费 flusher 无共享状态，排空顺序互不依赖）。
func (w *ErrLogWorker) Name() string { return "errlog" }

func (w *ErrLogWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("errlog worker: already started")
	}
	go func() {
		defer close(w.loopDone)
		w.loop(ctx)
	}()
	return nil
}

func (w *ErrLogWorker) loop(ctx context.Context) {
	t := time.NewTicker(w.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// 最终排空由 Close 以 shutdown 预算 ctx 执行（O2 停机纪律）——本
			// loop ctx 在 SIGTERM 即已取消，此处 flush 传它会恒截断丢全部明细；
			// 且在途 flush 为同步调用，loop 退出前必已收尾（无在途批次残留，
			// Close 无需 flushMu 等待——独立 worker 的简单性来源）。
			return
		case <-t.C:
			w.flush()
		}
	}
}

// EnqueueRejected 投递一条**拒绝行**（recordRejected：401/429/402/400/404 本地
// 拒绝 + 组限流）：普通队列——风暴采样丢弃面（非阻塞 select-default；丢弃
// 计数 DroppedReject 原子累加，供指标/日志对账）。
func (w *ErrLogWorker) EnqueueRejected(l *domain.UsageLog) {
	w.enqueue(w.rejectQ, l, &w.droppedReject, &w.warnReject, errlogDropWarnThreshold)
}

// EnqueueError 投递一条**双轨行**（finish/recordLog 的已计费错误：abort/failover/
// 4xx/5xx/network——usage_logs 错误行）：豁免队列——**不参与拒绝风暴采样丢弃，
// 恒落盘**（架构审查 B2；仅本队列自身溢出才丢——异常态，Warn 恰好一次）。
func (w *ErrLogWorker) EnqueueError(l *domain.UsageLog) {
	w.enqueue(w.exemptQ, l, &w.droppedExempt, &w.warnExempt, 1)
}

// enqueue 非阻塞有界投递（mu 短临界区：closed 检查 + select-default send——Close
// 置位后无残留窗口，S4）。队列满 → 丢弃 + 按类计数；累计 ≥ threshold 且边沿
// 未告警 → Warn 恰好一次（队列排空后回落，flush 内复位）。
func (w *ErrLogWorker) enqueue(q chan *domain.UsageLog, l *domain.UsageLog, dropped *atomic.Int64, warned *atomic.Bool, threshold int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		dropped.Add(1)
		return
	}
	select {
	case q <- l:
	default:
		n := dropped.Add(1)
		if n >= threshold && warned.CompareAndSwap(false, true) {
			if w.log != nil {
				w.log.Warn("errlog queue full, dropping (storm sampling)",
					logx.Int64("dropped", n), logx.Int64("threshold", threshold),
					logx.Int("queue_cap", cap(q)))
			}
		}
	}
}

// flush 排空队列中至多 BatchSize 行并批量落库（单写者：仅 loop 调用；Close 在
// loopDone 之后独占队列）。豁免队列优先（双轨行恒落盘语义：风暴下拒绝行让位
// 采样）。失败 → Warn + 该批丢弃（丢弃并入类计数）——审计明细非计费：DB 故障
// 不无限回灌卡死（与 Recorder 毒丸止损同向，此处直接丢弃不复灌）。
// 批次处理完成后两队列均空 → 丢弃告警边沿回落（S3——每风暴一次，不刷屏：
// 连续风暴期队列恒满不回落，风暴平息排空后下次风暴再告警）。
func (w *ErrLogWorker) flush() {
	batch := w.takeBatch()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), errlogFlushTimeout)
	defer cancel()
	if err := w.writer.InsertBatch(ctx, batch); err != nil {
		if w.log != nil {
			w.log.Warn("errlog batch insert failed, dropping batch", logx.Error(err))
		}
		w.dropBatch(batch)
	} else {
		w.inserted.Add(int64(len(batch)))
	}
	if len(w.exemptQ) == 0 && len(w.rejectQ) == 0 {
		w.warnReject.Store(false)
		w.warnExempt.Store(false)
	}
}

// dropBatch 落库失败止损：整批丢弃并计入**双轨丢弃**（错误行审计最重——失败
// 场景恰是审计需求时；丢弃类计数即对账指标）。
func (w *ErrLogWorker) dropBatch(batch []*domain.UsageLog) {
	w.droppedExempt.Add(int64(len(batch)))
}

// takeBatch 从两队列取至多 BatchSize 行（豁免优先；非阻塞轮询——队列空即返回；
// 单写者/Close 独占调用，无锁）。
func (w *ErrLogWorker) takeBatch() []*domain.UsageLog {
	batch := make([]*domain.UsageLog, 0, w.cfg.BatchSize)
	for len(batch) < w.cfg.BatchSize {
		select {
		case l := <-w.exemptQ:
			batch = append(batch, l)
		default:
			goto reject
		}
	}
reject:
	for len(batch) < w.cfg.BatchSize {
		select {
		case l := <-w.rejectQ:
			batch = append(batch, l)
		default:
			return batch
		}
	}
	return batch
}

// Close 幂等排空（优雅停机核心）：置位 closed（此后 Enqueue 丢弃计数——无尾
// 窗口静默丢，S4）→ 等 worker goroutine 退出（受预算约束；loop 退出前在途
// flush 已同步收尾）→ 两队列剩余条目分批落库（每批 ≤ BatchSize，预算内完整
// 排空——正常停机双轨行 + 未丢弃拒绝行不丢）；ctx 到期 → Warn（含已排空/剩余
// 条数）+ 截断退出（剩余丢弃计数），不阻塞停机。Close 结束打印 inserted/
// dropped 终值（S3 对账观测）。未 Start 也安全（跳过 loop 等待直接排空）。
func (w *ErrLogWorker) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true // 先置位：与 enqueue 互斥——此后投递全部丢弃计数，排空无竞态
		w.mu.Unlock()
		if w.started.Load() {
			select {
			case <-w.loopDone:
			case <-ctx.Done():
				if w.log != nil {
					w.log.Warn("errlog close: loop did not exit in time")
				}
			}
		}
		var flushed int64
		for {
			batch := w.takeBatch()
			if len(batch) == 0 {
				break
			}
			if ctx.Err() != nil { // 预算到期：截断退出（剩余丢弃计数；与 flusher
				// 截断 Warn 同族——错误审计明细非计费，截断可接受）
				w.dropBatch(batch)
				if w.log != nil {
					w.log.Warn("errlog close: shutdown budget exceeded, truncated drain",
						logx.Int64("flushed_logs", flushed), logx.Int64("remaining_logs", int64(len(batch))))
				}
				return
			}
			if err := w.writer.InsertBatch(ctx, batch); err != nil {
				w.dropBatch(batch)
				if w.log != nil {
					w.log.Warn("errlog close drain batch failed", logx.Error(err))
				}
				continue
			}
			w.inserted.Add(int64(len(batch)))
			flushed += int64(len(batch))
		}
		if w.log != nil {
			w.log.Info("errlog worker closed",
				logx.Int64("inserted_logs", w.inserted.Load()),
				logx.Int64("dropped_reject", w.droppedReject.Load()),
				logx.Int64("dropped_exempt", w.droppedExempt.Load()))
		}
	})
	return nil
}

// DroppedReject 拒绝行丢弃计数（队列满采样；统计面对账指标）。
func (w *ErrLogWorker) DroppedReject() int64 { return w.droppedReject.Load() }

// DroppedExempt 双轨行丢弃计数（恒 0 为正常——豁免队列溢出/落库失败止损；
// >0 即异常态观测）。
func (w *ErrLogWorker) DroppedExempt() int64 { return w.droppedExempt.Load() }

// Inserted 成功落盘计数（测试/指标观测）。
func (w *ErrLogWorker) Inserted() int64 { return w.inserted.Load() }

// Queued 当前两队列积压总条数（背压观测：恒 ≤ QueueSize + ExemptQueueSize）。
func (w *ErrLogWorker) Queued() int { return len(w.rejectQ) + len(w.exemptQ) }
