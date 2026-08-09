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
	Workers                int           // flush 并行 worker 数（0 = 单 worker）
}

// pendingWaterline pending 日志条数水线：超过 → Warn（可观测，非反压——Record
// 永不阻塞，pending 内存即唯一积压面）。按聚合日志条数计（评审 C-1：429 风暴
// 24.5k 日志/s 才是无界增长场景；按去重用户数计 ≤1M 用户不可达恒不告警）。
// var（非 const）：测试注入小阈值，默认 1M 不变，后续可配置化。
var pendingWaterline int64 = 1_000_000

// flusherPending 单用户聚合条目（userID → cost 总额 + 明细日志，同事务落库）。
type flusherPending struct {
	cost int64
	logs []*domain.UsageLog
}

// Flusher 计费批量落库（worker.Worker 契约，Name="billing"）。O1 管道化：
// Record 只做统计聚合（stats.Aggregate——billed 流量进 usagestat 统计面，与
// 非 billed 一视同仁，每日志恰好一个写者）+ 短锁归并 pending map（userID →
// cost+logs），O(1) 摊还，**永不阻塞**（无 channel——此前有界 channel cap
// 16384 饱和阻塞在 proxy.finish() 内是压测 3.75k/s 塌陷根因）。flush 单入口
// 串行（flushMu：ticker/ctx.Done/Close 三处触发共用，杜绝并发换批）：锁内
// swap 整个 pending（换新 map，flush 期间新日志进新 map 零阻塞）→ 批按
// userID 分片（同 user 恒同桶 → 实例内串行；FEFO 行锁跨实例安全不变）→
// N worker 并发逐 user DeductAndLog 单事务 → 成功定向刷新余额快照（O(1)）。
// Close 幂等：排空 + 最后全量 flush + 等全部在途 worker 批（flushMu 串行 +
// 批内 wg）——优雅停机核心：在途请求已由 waitForInflight 收敛，pending 即
// 全部计费，不丢。
type Flusher struct {
	cfg      FlushConfig
	stats    *usage.Recorder
	writer   DeductWriter
	bal      *Balances
	log      *logx.Logger
	workers  int
	mu       sync.Mutex // 保护 pending（Record 聚合与 flush 换批/回灌并发）
	pending  map[int64]*flusherPending
	pendingN atomic.Int64 // pending 日志条数（水线观测；换批/回灌同步增减）
	warned   atomic.Bool  // 水线越过告警边沿（回落复位，避免重复刷屏）
	flushMu  sync.Mutex   // 单 flush 入口串行：ticker/ctx.Done/Close 三处触发互斥
	started  atomic.Bool
	loopDone chan struct{}
	closeOnce sync.Once
}

func NewFlusher(cfg FlushConfig, writer DeductWriter, stats *usage.Recorder, bal *Balances, log *logx.Logger) *Flusher {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 1
	}
	return &Flusher{
		cfg: cfg, stats: stats, writer: writer, bal: bal, log: log,
		workers:  workers,
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
			f.flushCtx(ctx) // 退出前最后一次落库尝试（ctx 已取消时即截断放弃——排空由 Close 以 shutdown 预算约束执行）
			return
		case <-flushT.C:
			f.flush()
		case <-refreshT.C:
			_ = f.bal.Reload(context.Background()) // fail-safe：内部 Warn + 保留旧快照
		}
	}
}

// Record 记录一条计费日志（proxy shouldBill 路由的 billed 路径）：统计聚合 +
// 短锁归并 pending map。O1 管道化：无 channel，**永不阻塞**（HTTP 层过载保护
// 不再依赖反压——pending 内存由水线 Warn 观测，崩溃丢 ≤1 flush 窗口语义不变）。
func (f *Flusher) Record(l *domain.UsageLog) {
	f.stats.Aggregate(l)
	f.mu.Lock()
	e, ok := f.pending[l.UserID]
	if !ok {
		e = &flusherPending{}
		f.pending[l.UserID] = e
	}
	e.cost += l.Cost
	e.logs = append(e.logs, l)
	n := f.pendingN.Add(1)
	f.mu.Unlock()
	if n > pendingWaterline && f.warned.CompareAndSwap(false, true) {
		if f.log != nil {
			f.log.Warn("billing pending exceeds waterline", logx.Int64("pending_logs", n), logx.Int64("waterline", pendingWaterline))
		}
	}
}

// Close 幂等排空（优雅停机核心）：等聚合 goroutine 退出（其 ctx.Done 路径已
// flush 一次）→ 受 shutdown ctx 预算约束的排空循环（flushMu 串行：与在途
// ticker flush 互斥，先等其 worker 批完成——无并发换批、无在途批次残留）。
// 正常情形完整排空语义不变（无 deadline ctx = 全部落库）；ctx 到期 → Warn
// （含已排空/剩余条数）+ 截断退出，不阻塞停机（O1 复测：44k/s 压测后 1.7M
// pending 无预算排空需数分钟）。未 Start 也安全（跳过等待；pending 残留同样
// 排空）。
func (f *Flusher) Close(ctx context.Context) error {
	f.closeOnce.Do(func() {
		if f.started.Load() {
			select {
			case <-f.loopDone:
			case <-ctx.Done():
				if f.log != nil {
					f.log.Warn("billing flusher close: aggregator did not exit in time")
				}
			}
		}
		var flushed int64
		for f.pendingCount() > 0 {
			if ctx.Err() != nil { // 预算到期：截断退出（剩余条目由 flushCtx 截断回灌，丢 ≤1 flush 窗口；remaining_logs 用 pendingN 日志条数与 flushed_logs 单位一致——pendingCount 为去重用户数会低估，评审 I-1）
				if f.log != nil {
					f.log.Warn("billing flusher close: shutdown budget exceeded, truncated drain",
						logx.Int64("flushed_logs", flushed), logx.Int64("remaining_logs", f.pendingN.Load()))
				}
				return
			}
			flushed += f.flushCtx(ctx)
		}
	})
	return nil
}

func (f *Flusher) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

// flush 全量落库（ticker 路径，无预算约束）。
func (f *Flusher) flush() { f.flushCtx(context.Background()) }

// flushCtx 受 ctx 约束的全量落库（单入口：ticker/ctx.Done/Close 三处触发共用，
// flushMu 串行——杜绝并发换批；DB 写锁外）：锁内 swap 整个 pending → 批按
// userID 分片（同 user 恒同桶 → 实例内串行）→ N worker 并发逐 user
// DeductAndLog 单事务；成功 → bal.Set 定向刷新余额快照（O(1) 原地 Store）；
// 失败 → Warn + cost+logs 一起回灌当前 pending（评审 C-2：只回 cost 丢日志——
// 明细与扣费必须同批重试，否则重试后扣费无明细）。返回前等待本批全部 worker
// 完成（Close 由此无在途批次）。O1 收尾：逐 user 处理前检查 ctx，预算到期即
// 截断——未处理条目原样回灌（不丢、由 Close 决定放弃），在途事务经 ctx 取消
// 快速失败；返回本批成功落库日志条数（Close 汇总作 Warn 诊断）。
func (f *Flusher) flushCtx(ctx context.Context) int64 {
	f.flushMu.Lock()
	defer f.flushMu.Unlock()

	f.mu.Lock()
	if len(f.pending) == 0 {
		f.mu.Unlock()
		return 0
	}
	pend := f.pending
	f.pending = make(map[int64]*flusherPending)
	var batchLogs int64
	for _, e := range pend {
		batchLogs += int64(len(e.logs))
	}
	f.pendingN.Add(-batchLogs)
	if f.pendingN.Load() < pendingWaterline {
		f.warned.Store(false)
	}
	f.mu.Unlock()

	// 按 userID 分片：同 user 恒同桶（实例内串行——FEFO 条件扣费行锁跨实例
	// 安全不变）。
	shards := make([]map[int64]*flusherPending, f.workers)
	for i := range shards {
		shards[i] = make(map[int64]*flusherPending)
	}
	for uid, e := range pend {
		shards[uint64(uid)%uint64(f.workers)][uid] = e
	}

	var wg sync.WaitGroup
	var drained atomic.Int64
	for _, shard := range shards {
		if len(shard) == 0 {
			continue
		}
		wg.Add(1)
		go func(s map[int64]*flusherPending) {
			defer wg.Done()
			for uid, e := range s {
				if ctx.Err() != nil { // 预算到期：截断，未处理回灌（Close 据此 Warn 后放弃）
					f.refill(uid, e)
					continue
				}
				_, bal, err := f.writer.DeductAndLog(ctx, uid, e.cost, e.logs)
				if err != nil {
					if f.log != nil {
						f.log.Warn("billing deduct failed", logx.Int64("user_id", uid), logx.Int64("cost", e.cost), logx.Error(err))
					}
					f.refill(uid, e)
					continue
				}
				f.bal.Set(uid, bal)
				drained.Add(int64(len(e.logs)))
			}
		}(shard)
	}
	wg.Wait()
	return drained.Load()
}

// refill 失败回灌：该 user 的 cost+logs 合并回当前 pending（锁内 append——flush
// 期间 Record 进新 map，回灌与 Record 并发安全）。
func (f *Flusher) refill(uid int64, e *flusherPending) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pe, ok := f.pending[uid]
	if !ok {
		pe = &flusherPending{}
		f.pending[uid] = pe
	}
	pe.cost += e.cost
	pe.logs = append(pe.logs, e.logs...)
	f.pendingN.Add(int64(len(e.logs)))
}
