package billing

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/usage"
	"go-proxy-mini/pkg/logx"
)

// DeductWriter 扣费落库面（repository.Repository 实现）：单事务 FEFO 条件扣费
// + 同批计费日志（DeductAndLog）。
type DeductWriter interface {
	DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (overdrafted bool, balanceAfter int64, err error)
}

// FlushConfig flusher 节奏（config.BillingConfig 映射）。
type FlushConfig struct {
	FlushInterval          time.Duration // 扣费落库周期
	BalanceRefreshInterval time.Duration // 余额快照全量刷新周期
}

// flusherPending 单用户聚合条目（userID → cost 总额 + 明细日志，同事务落库）。
type flusherPending struct {
	cost int64
	logs []*domain.UsageLog
}

// Flusher 计费批量落库（worker.Worker 契约，Name="billing"）：
// Record 只聚合——统计面复用 usage.Recorder.Aggregate（billed 与非 billed
// 统计一视同仁，close 顺序保证 Aggregate 先于 stats flush）+ 明细入有界
// channel（cap 16384 饱和阻塞反压，与 rec.logCh 同型）→ 聚合 goroutine 维护
// pending map（userID → cost+logs）→ flush 周期逐 user 单事务扣费落库 →
// 成功定向刷新余额快照。Close 排空 channel + 最后一次全量 flush（优雅停机
// 核心：在途请求已由 waitForInflight 收敛，pending 即全部计费，不丢）。
type Flusher struct {
	cfg       FlushConfig
	stats     *usage.Recorder
	writer    DeductWriter
	bal       *Balances
	log       *logx.Logger
	ch        chan *domain.UsageLog
	mu        sync.Mutex // 保护 pending（聚合 goroutine 与 Close 排空并发）
	pending   map[int64]*flusherPending
	started   atomic.Bool
	loopDone  chan struct{}
	closeOnce sync.Once
}

func NewFlusher(cfg FlushConfig, writer DeductWriter, stats *usage.Recorder, bal *Balances, log *logx.Logger) *Flusher {
	return &Flusher{
		cfg: cfg, stats: stats, writer: writer, bal: bal, log: log,
		ch:       make(chan *domain.UsageLog, 16384),
		pending:  make(map[int64]*flusherPending),
		loopDone: make(chan struct{}),
	}
}

// Name worker.Worker 契约（wm 按注册反向排空：flusher 最后注册最先排空）。
func (f *Flusher) Name() string { return "billing" }

func (f *Flusher) Start(ctx context.Context) error {
	if !f.started.CompareAndSwap(false, true) {
		return fmt.Errorf("billing flusher: already started")
	}
	go func() {
		defer close(f.loopDone)
		f.loop(ctx)
	}()
	return nil
}

func (f *Flusher) loop(ctx context.Context) {
	flushT := time.NewTicker(f.cfg.FlushInterval)
	defer flushT.Stop()
	refreshT := time.NewTicker(f.cfg.BalanceRefreshInterval)
	defer refreshT.Stop()
	for {
		select {
		case <-ctx.Done():
			f.flush() // 退出前最后一次落库（Close 还会兜底全量 flush，幂等）
			return
		case <-flushT.C:
			f.flush()
		case <-refreshT.C:
			_ = f.bal.Reload(context.Background()) // fail-safe：内部 Warn + 保留旧快照
		case l := <-f.ch:
			f.aggregate(l)
		}
	}
}

// Record 记录一条计费日志（proxy shouldBill 路由的 billed 路径）：统计聚合
// + 明细入有界 channel。常时非阻塞；饱和时阻塞反压（不得丢数据，用户决策
// 2026-08-05 同 rec.logCh 语义；HTTP 层过载保护兜底）。
func (f *Flusher) Record(l *domain.UsageLog) {
	f.stats.Aggregate(l)
	f.ch <- l
}

// Close 幂等排空（优雅停机核心）：排空 channel 剩余 → 等聚合 goroutine 退出
// （其 ctx.Done 路径已 flush 一次）→ 最后一次全量 flush，失败回灌后继续重试
// 直至清空或 ctx 预算耗尽（超时 Warn——极限情况丢 ≤1 flush 窗口，可接受）。
// 未 Start 也安全（跳过等待；channel 残留同样排空）。
func (f *Flusher) Close(ctx context.Context) error {
	f.closeOnce.Do(func() {
		if f.started.Load() {
			f.drain()
			select {
			case <-f.loopDone:
			case <-ctx.Done():
				if f.log != nil {
					f.log.Warn("billing flusher close: aggregator did not exit in time")
				}
			}
		} else {
			f.drain()
		}
		for f.pendingCount() > 0 {
			f.flush()
			select {
			case <-ctx.Done():
				if f.log != nil {
					f.log.Warn("billing flusher close: pending billing not fully flushed")
				}
				return
			default:
			}
		}
	})
	return nil
}

// drain 非阻塞排空 channel 至空（Close 用；与聚合 goroutine 竞争消费——channel
// 语义保证每个日志恰好被一方聚合）。
func (f *Flusher) drain() {
	for {
		select {
		case l := <-f.ch:
			f.aggregate(l)
		default:
			return
		}
	}
}

// aggregate 归并单用户（聚合 goroutine 与 Close 排空共用；mu 串行）。
func (f *Flusher) aggregate(l *domain.UsageLog) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.pending[l.UserID]
	if !ok {
		e = &flusherPending{}
		f.pending[l.UserID] = e
	}
	e.cost += l.Cost
	e.logs = append(e.logs, l)
}

func (f *Flusher) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

// flush 全量落库（ticker/ctx.Done/Close 共用；mu 串行换批，DB 写锁外）：逐
// user 调 writer.DeductAndLog 单事务；成功 → bal.Set 定向刷新余额快照；失败 →
// Warn + cost+logs 一起回灌（评审 C-2：只回 cost 丢日志——明细与扣费必须同批
// 重试，否则重试后扣费无明细）。
func (f *Flusher) flush() {
	f.mu.Lock()
	if len(f.pending) == 0 {
		f.mu.Unlock()
		return
	}
	pend := f.pending
	f.pending = make(map[int64]*flusherPending)
	f.mu.Unlock()

	for uid, e := range pend {
		_, bal, err := f.writer.DeductAndLog(context.Background(), uid, e.cost, e.logs)
		if err != nil {
			if f.log != nil {
				f.log.Warn("billing deduct failed", logx.Int64("user_id", uid), logx.Int64("cost", e.cost), logx.Error(err))
			}
			f.mu.Lock()
			pe, ok := f.pending[uid]
			if !ok {
				pe = &flusherPending{}
				f.pending[uid] = pe
			}
			pe.cost += e.cost
			pe.logs = append(pe.logs, e.logs...)
			f.mu.Unlock()
			continue
		}
		f.bal.Set(uid, bal)
	}
}
