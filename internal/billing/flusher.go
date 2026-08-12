// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/logx"
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

// maxUsageLogsPerTx 单用户单事务日志行数上限（P2，压测 2026-08-11 修复）：
// P1 故障期单用户积压 1M+ 行 → 单事务 2000+ 分片（500 行/批）串行插入 8 分钟
// （xact_age 08:02 实证），flushMu 串行冻结全局 usage 记录 + 堆涨 4.6GB（巨批
// CreateBulk 构建内存）。超限拆多事务逐事务提交——每事务 ≤ 10k 行（~5 批/
// 事务，单事务时长/内存有界），事务内仍由 DeductAndLog 按 2000 行/批分片。
// 热点修复 A（2026-08-11，测量数据见 repository/pg_deduct_bench_test.go）：
// 保持 10k——档位 2k/5k/10k 逐行墙钟持平（27/23/22µs·行），drain 能力
// （预算 500ms 内可提交块数 × 块行数，线性缩放下）10k 档恒 ≥ 小档。
const maxUsageLogsPerTx = 10_000

// backlogDrainBudget 单次 flush 内积压用户续传循环的时间预算（P2a，压测
// 2026-08-11 复测修复）：P2 拆事务的"每 flush 每用户至多一块"把单用户 drain
// 钉死在 10k 行/s（1s flush 周期 × 10k 块），持续超限到达（429 风暴/单 key
// 高压）时 pending 无界增长（60s 风暴 → 9.8M 行 / RSS 7.5GB）。续传循环
// （见 flushCtx）：逐块（≤ maxUsageLogsPerTx，事务内存有界不变）提交，块间
// 续取（含 flush 期间并发 Record/回灌），超预算 → 剩余 refill 下轮续传。
// 效果：单用户 drain 由"一块/次 flush"提升到预算内尽可能多的块（DB 快时
// 5-10 倍），同时 flushMu 持有时间有界（≤ 预算 + 尾事务）——其他用户 flush
// 周期不被长期饿死（P2"多用户计费不受单用户积压影响"不变量保持）。
// var（非 const）：测试注入小预算；默认 500ms（= 1s flush 周期的一半）。
var backlogDrainBudget = 500 * time.Millisecond

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
// N worker 并发逐 user DeductAndLog 单事务（P2：单用户巨批 > maxUsageLogsPerTx
// 行拆多事务逐事务提交；P2a：积压用户续传循环——逐块提交后块间续取，至多
// backlogDrainBudget 预算（flushMu 持有时间有界，其他用户 flush 周期不被
// 长期饿死；单用户 drain 由 10k/次 flush 提升到 DB 实际吞吐））→ 成功定向
// 刷新余额快照（O(1)）。
// Close 幂等：等聚合 goroutine 退出 + 受 shutdown ctx 预算约束的排空循环，
// 其中"等在途批次"以 flushMu 获取表达（flushCtx 串行——在途批次持有
// flushMu 期间 Close 等待；SIGTERM 时 ticker 批次可能已在途，若无此等待
// drain 循环见 pendingCount()==0 会静默提前返回、"无在途批次残留"不变量被
// 破坏）——优雅停机核心：在途请求已由 waitForInflight 收敛，pending 即全部
// 计费，不丢。ticker 批次用 baseCtx（可取消）：Close 预算到期 → Cancel →
// 在途 DeductAndLog 快速失败（回灌不丢），不无界阻塞停机（O1 复测：在途
// 批次 Background ctx 令停机拖至分钟级）。
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
	// lastFlush 最近一次实际 flush 周期完成时刻（UnixMilli；0 = 尚未 flush——
	// 空 pending 的早退路径不记，观测"flusher 最近何时真正落过库"）。
	lastFlush atomic.Int64
	flushMu   sync.Mutex // 单 flush 入口串行：ticker/ctx.Done/Close 三处触发互斥；在途批次即其持有者
	started   atomic.Bool
	loopDone  chan struct{}
	closeOnce sync.Once
	// O2 停机：ticker 路径批次的可取消父 ctx（常时 = Background 语义；Close
	// 预算到期 Cancel → 在途批次快速失败）。baseCtx 仅经 baseCancel 修改
	// （Close 内单写者），loop/Close 并发读安全。
	baseCtx    context.Context
	baseCancel context.CancelFunc
}

func NewFlusher(cfg FlushConfig, writer DeductWriter, stats *usage.Recorder, bal *Balances, log *logx.Logger) *Flusher {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 1
	}
	f := &Flusher{
		cfg: cfg, stats: stats, writer: writer, bal: bal, log: log,
		workers:  workers,
		pending:  make(map[int64]*flusherPending),
		loopDone: make(chan struct{}),
	}
	f.baseCtx, f.baseCancel = context.WithCancel(context.Background())
	return f
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

// Close 幂等排空（优雅停机核心）：等聚合 goroutine 退出（受预算约束）→ 以
// flushMu 获取等待在途批次（SIGTERM 时 ticker 批次可能已在途占住 flushMu 且
// pending 已 swap；Close 必须先等其结束，否则 drain 循环见 pendingCount()==0
// 会静默提前返回，在途批次带着计费日志无界运行——O1 复测根因 1）→ 受
// shutdown ctx 预算约束的排空循环（此时无在途批次、flushMu 无竞争）。正常
// 情形完整排空语义不变（无 deadline ctx = 全部落库）；ctx 到期 → Cancel
// baseCtx（在途批次 DeductAndLog 快速失败回灌，不丢）+ Warn（含已排空/剩余
// 条数）+ 截断退出，不阻塞停机（O1 复测：44k/s 压测后 1.7M pending 无预算
// 排空需数分钟）。未 Start 也安全（跳过聚合等待；在途 flush 与 pending 残留
// 同样等待/排空）。
func (f *Flusher) Close(ctx context.Context) error {
	f.closeOnce.Do(func() {
		defer f.baseCancel() // flusher 关闭后 baseCtx 不得再有存活批次
		if f.started.Load() {
			// 等聚合 goroutine 退出（受预算约束）。SIGTERM 时 loop 可能阻塞在
			// ticker flush（baseCtx 批次在途）——loopDone 待其批次结束 + 末次
			// flush 后才关闭；预算到期 → Warn + 继续（在途批次由下面 flushMu
			// 等待强制取消）。
			select {
			case <-f.loopDone:
			case <-ctx.Done():
				if f.log != nil {
					f.log.Warn("billing flusher close: aggregator did not exit in time")
				}
			}
		}
		// 等在途批次（有界）：flushCtx 由 flushMu 串行——"是否有批次在途"即
		// "flushMu 是否被占"；尝试获取 flushMu：拿到即无在途批次（其退出前
		// 必释放），预算内等其自然完成（完整排空语义不变）；到期 → Cancel
		// baseCtx 强制在途 DeductAndLog 快速失败（回灌不丢），等批次收尾后
		// 走截断路径。未 Start 时无竞争立即拿到（此前测试直接调 flush 的
		// 在途批次同样被等待）。
		acquired := make(chan struct{})
		go func() { f.flushMu.Lock(); close(acquired) }()
		select {
		case <-acquired:
			f.flushMu.Unlock()
		case <-ctx.Done():
			f.baseCancel()
			<-acquired // 取消后在途批次快速收尾（DeductAndLog 尊重 ctx）
			f.flushMu.Unlock()
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

// flush 全量落库（ticker 路径，无预算约束——常时与 Background 等价；Close
// 预算到期时 Cancel baseCtx，在途批次快速失败）。
func (f *Flusher) flush() { f.flushCtx(f.baseCtx) }

// flushCtx 受 ctx 约束的全量落库（单入口：ticker/ctx.Done/Close 三处触发共用，
// flushMu 串行——杜绝并发换批；DB 写锁外）：锁内 swap 整个 pending → 批按
// userID 分片（同 user 恒同桶 → 实例内串行）→ N worker 并发逐 user
// DeductAndLog 单事务（P2：单用户巨批拆事务，见 worker 循环）；成功 →
// bal.Set 定向刷新余额快照（O(1) 原地 Store）；失败 → Warn + cost+logs 一起
// 回灌当前 pending（评审 C-2：只回 cost 丢日志——明细与扣费必须同批重试，
// 否则重试后扣费无明细）。返回前等待本批全部 worker 完成（Close 由此以
// flushMu 获取等待无在途批次）。O1 收尾：逐 user 处理前检查 ctx，预算到期即
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
				// P2a（压测 2026-08-11 复测修复）：积压用户续传循环。P2 拆
				// 事务的"每 flush 每用户至多一块"把单用户 drain 钉死在
				// 10k 行/s（1s flush 周期），持续超限到达时 pending 无界
				// 增长（9.8M 行 / 7.5GB 实证）。循环：逐块（≤
				// maxUsageLogsPerTx，单事务时长/内存有界不变）提交，块间
				// 续取（takePending——flush 期间并发 Record/失败回灌的条目
				// 一并续传），超过 backlogDrainBudget 或 ctx 到期 → 剩余
				// refill 由下轮 flush/Close 续传（flushMu 持有时间有界，
				// 其他用户 flush 周期不被长期饿死）。失败 → 本用户本轮
				// 停止（chunk 已回灌，下轮重试；不无界重试同一块）。每
				// 事务原子；跨事务部分成功可接受（评审裁决：宁可少记
				// 不可死锁）——失败仅回灌未提交块，已提交块不重放（不
				// 重复扣费）。
				for start := time.Now(); e != nil; e = f.takePending(uid) {
					if ctx.Err() != nil || time.Since(start) > backlogDrainBudget {
						f.refill(uid, e)
						break
					}
					chunk := e
					if len(e.logs) > maxUsageLogsPerTx {
						chunk = &flusherPending{
							cost: e.cost * maxUsageLogsPerTx / int64(len(e.logs)),
							logs: e.logs[:maxUsageLogsPerTx],
						}
						f.refill(uid, &flusherPending{
							cost: e.cost - chunk.cost,
							logs: e.logs[maxUsageLogsPerTx:],
						})
					}
					_, bal, err := f.writer.DeductAndLog(ctx, uid, chunk.cost, chunk.logs)
					if err != nil {
						if f.log != nil {
							f.log.Warn("billing deduct failed", logx.Int64("user_id", uid), logx.Int64("cost", chunk.cost), logx.Error(err))
						}
						f.refill(uid, chunk)
						break
					}
					f.bal.Set(uid, bal)
					drained.Add(int64(len(chunk.logs)))
				}
			}
		}(shard)
	}
	wg.Wait()
	f.lastFlush.Store(time.Now().UnixMilli())
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

// takePending 锁内取走某用户的当前 pending 条目（无则 nil）——P2a 续传循环
// 用：积压用户逐块提交后立即续取（flush 期间并发 Record 入新 map / 失败回灌
// 的条目同样被续传），单次 flush 内尽可能多落库。与 refill/Record 同锁，
// pendingN 同步增减（条目已从 map 移除即不再计入水线观测）。
func (f *Flusher) takePending(uid int64) *flusherPending {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.pending[uid]
	if !ok {
		return nil
	}
	delete(f.pending, uid)
	f.pendingN.Add(-int64(len(e.logs)))
	return e
}
