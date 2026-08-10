// Package usage 承载请求明细的异步落库与预聚合统计（规格 §7.2/§10.5）以及
// usagelog 保留策略（retention worker，Phase 5 T4.5：按日分区 DROP 清理）。
// 统计聚合永不失真（同步进内存计数），明细经无界 pending 批量落库（O1 管道化：
// Record 永不阻塞——此前有界 channel cap 16384 饱和阻塞发送是压测 off 路径
// 16.4k goroutine 卡 chan send、healthz inflight 31-33k @10k 幽灵根因，O3
// 复测定位 2026-08-09；崩溃丢 ≤1 flush 窗口的崩溃等价语义不变，pending 内存
// 即唯一积压面，由水线 Warn 观测）。
package usage

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

type UsageConfig struct {
	BatchSize          int
	FlushInterval      time.Duration
	StatsFlushInterval time.Duration
	Workers            int // flush 并行 worker 数（0 = 单 worker；O1 模式分片并行）
}

type LogInserter interface {
	InsertBatch(ctx context.Context, logs []*domain.UsageLog) error
}

type StatUpserter interface {
	Upsert(ctx context.Context, buckets []*domain.StatBucket) error
}

// QuotaWriter 批量回写 key 额度消耗（增量；内存权威，DB 滞后 ≤ flush 间隔）。
// 由 proxy 的 gate 计数 + 本 Recorder 的 flush 节奏落库（Phase 3a：额度后扣）。
type QuotaWriter interface {
	AddQuotaUsed(ctx context.Context, deltas map[int64]int64) error
}

// statBatchSize / quotaBatchSize 单条批量 SQL 的桶/键数（PG 参数上限 65535：
// stat 500 × 17 列 ≈ 8.5k 参数；quota 500 ×（CASE 两参数 + IN 一参数）≈ 1.5k，
// 均安全）。usage 层负责分块——失败回灌以块为原子单位，防部分成功重复计数。
const (
	statBatchSize  = 500
	quotaBatchSize = 500
	// maxStatsDeadlockRetries 统计 Upsert 死锁（40P01）重试上限（不含首次尝试）。
	// 死锁瞬时（对端事务被 PG 回滚即释放锁）——重试 2 次足够消化并发窗口；仍
	// 失败 → 回灌等下次 flush 再试（不丢），不无限重试拖长 flushMu 占锁。
	maxStatsDeadlockRetries = 2
)

// pendingWaterline 明细 pending 条数水线：超过 → Warn（可观测，非反压——
// Record 永不阻塞，pending 内存即唯一积压面；与 billing Flusher 同模式）。
// var（非 const）：测试注入小阈值，默认 1M 不变，后续可配置化。
var pendingWaterline int64 = 1_000_000

// maxLogFlushFailures 毒丸行止损阈值（评审 I-3）：单 shard 连续 flush 失败
// ≥ 此数 → 显式丢弃该失败 chunk（Error 日志 + 首行 request_id），不再无限
// 回灌卡死该 shard（旧实现：一行永久失败则 workers=1 时整管道吞吐归零）。
// var（非 const）：测试注入小阈值。
var maxLogFlushFailures = 5

type Recorder struct {
	cfg       UsageConfig
	logs      LogInserter
	stats     StatUpserter
	quota     QuotaWriter // 可选（nil = 不回写额度）
	log       *logx.Logger
	workers   int
	mu        sync.Mutex // 保护 pending/counters/quotaUsed（Record 聚合与 flush 换批/回灌并发）
	pending   []*domain.UsageLog
	counters  map[statBucketKey]*statCounters
	quotaUsed map[int64]int64 // key_id → 待回写 token 增量
	pendingN  atomic.Int64    // pending 明细条数（水线观测 + Close Warn 单位；换批/回灌同步增减）
	warned    atomic.Bool     // 水线越过告警边沿（回落复位，避免重复刷屏）
	// flushMu 单 flush 入口串行：日志 flush（flushLogs）与统计 flush
	// （flushStats）共用同一互斥锁——Close 的在途屏障需要（"是否有批次在途"
	// 即"flushMu 是否被占"），这是单一互斥锁的代价（评审 I-1 耦合）：DB 故障
	// 恢复后日志积压巨大时，单次 flushLogs 占锁可致额度回写/统计 Upsert 整体
	// 排队、延迟同幅放大（额度持久化滞后，**非丢数据**）。ticker/Close 两处
	// 触发互斥；在途批次即其持有者。
	flushMu sync.Mutex
	// failCounts 分片级连续 flush 失败计数（毒丸行止损，I-3）：flushLogs 失败
	// 路径自增、成功推进复位；仅 flush 失败/成功路径写（Record 热路径零触碰）。
	// 安全：flushLogs 由 flushMu 串行，单次调用内每分片恰一个 goroutine 写
	// 自己的槽位，wg.Wait 后才进入下一轮 flush。
	failCounts  []int
	startOnce   atomic.Bool
	loopDone    chan struct{} // Start 的两个 loop 全部退出后关闭
	closeOnce   sync.Once
	closed      atomic.Bool // Close 完成后置位（I-4）：后续 Record 走 Warn 一次路径
	closeWarned atomic.Bool // closed 后首次 Record 的 Warn 边沿（只告警一次，防刷屏）
	// O2 停机：ticker 路径批次的可取消父 ctx（常时 = Background 语义；Close
	// 预算到期 Cancel → 在途落库快速失败回灌，不丢）。baseCtx 仅经 baseCancel
	// 修改（Close 内单写者），loop/Close 并发读安全。
	baseCtx    context.Context
	baseCancel context.CancelFunc
}

type statCounters struct {
	bucket domain.StatBucket
}

// statBucketKey 统计桶唯一键（可比较 struct，GC 削减 P4：替代 fmt.Sprintf 键
// ——聚合/回灌零分配，且缩短 Recorder/Flusher 的 mu 临界区；与聚合/回灌同一
// 类型保证合并到原桶）。
type statBucketKey struct {
	hourUnix   int64
	groupID    int64
	accountID  int64
	templateID int64
	userID     int64
	model      string
	isErr      bool
}

func New(cfg UsageConfig, logs LogInserter, stats StatUpserter, log *logx.Logger) *Recorder {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 1
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1
	}
	r := &Recorder{
		cfg:        cfg,
		logs:       logs,
		stats:      stats,
		log:        log,
		workers:    workers,
		failCounts: make([]int, workers),
		counters:   make(map[statBucketKey]*statCounters),
		quotaUsed:  make(map[int64]int64),
		loopDone:   make(chan struct{}),
	}
	r.baseCtx, r.baseCancel = context.WithCancel(context.Background())
	return r
}

// SetQuotaWriter 注入额度回写器（装配期调用；nil = 关闭回写）。
func (r *Recorder) SetQuotaWriter(q QuotaWriter) {
	r.mu.Lock()
	r.quota = q
	r.mu.Unlock()
}

// Name 满足 worker.Worker 契约（Global Constraints #5）；重复 Start 幂等。
func (r *Recorder) Name() string { return "usage" }

func (r *Recorder) Start(ctx context.Context) error {
	if !r.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("recorder: already started")
	}
	go func() {
		defer close(r.loopDone)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); r.logWriterLoop(ctx) }()
		go func() { defer wg.Done(); r.statsFlushLoop(ctx) }()
		wg.Wait()
	}()
	return nil
}

// Record 记录一次请求：统计同步聚合（永不丢弃）+ 短锁归并 pending（无界 slice
// append，O(1) 摊还）——**永不阻塞**（无 channel：此前有界 channel cap 16384
// 饱和阻塞发送是 off 路径 16.4k goroutine 卡 chan send 幽灵根因；HTTP 层过载
// 保护由 max_inflight 兜底，pending 内存由水线 Warn 观测，崩溃丢 ≤1 flush
// 窗口语义不变）。热路径零额外开销：closed 检查为 1 次 atomic.Load（I-4）。
func (r *Recorder) Record(l *domain.UsageLog) {
	if r.closed.Load() { // Close 完成后无消费者——防御性缺口（评审 I-4）：
		// Warn 恰好一次（不刷屏）；明细仍聚合入 pending **不丢**（驻留内存由
		// 本 Warn 观测，worker 管理器顺序保证正常停机不触发）。
		if r.closeWarned.CompareAndSwap(false, true) && r.log != nil {
			r.log.Warn("usage record after close: detail retained in memory (no consumer)",
				logx.String("request_id", l.RequestID))
		}
	}
	r.Aggregate(l)
	r.mu.Lock()
	r.pending = append(r.pending, l)
	n := r.pendingN.Add(1)
	r.mu.Unlock()
	if n > pendingWaterline && r.warned.CompareAndSwap(false, true) {
		if r.log != nil {
			r.log.Warn("usage pending exceeds waterline", logx.Int64("pending_logs", n), logx.Int64("waterline", pendingWaterline))
		}
	}
}

// Aggregate 同步聚合统计（请求数/错误/tokens/cost 进 StatBucket，不入明细
// channel）——T3 计费 Flusher 复用同一聚合（billed 请求只经 Flusher 不落本
// 明细；每日志恰好一个写者）。与 Record 等价，仅跳过明细投递。
func (r *Recorder) Aggregate(l *domain.UsageLog) {
	r.aggregate(l)
}

// Pending 返回尚未落库的明细条数（测试与积压观测用）。
func (r *Recorder) Pending() int { return int(r.pendingN.Load()) }

func (r *Recorder) aggregate(l *domain.UsageLog) {
	hour := l.CreatedAt.UTC().Truncate(time.Hour)
	isErr := l.ErrorType != domain.ErrNone
	key := statBucketKeyOf(hour.Unix(), l.GroupID, l.AccountID, l.TemplateID, l.UserID, l.Model, isErr)
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[key]
	if !ok {
		c = &statCounters{bucket: domain.StatBucket{
			BucketTime: hour, GroupID: l.GroupID, AccountID: l.AccountID,
			TemplateID: l.TemplateID, UserID: l.UserID, Model: l.Model, IsError: isErr,
		}}
		r.counters[key] = c
	}
	// quota_used 增量聚合（key 级；Recorder 节奏批量回写，内存权威在 proxy gate）
	if l.KeyID > 0 {
		r.quotaUsed[l.KeyID] += l.TotalTokens
	}
	c.bucket.RequestCount++
	if isErr {
		c.bucket.ErrorCount++
	}
	c.bucket.InputTokens += l.InputTokens
	c.bucket.OutputTokens += l.OutputTokens
	c.bucket.TotalTokens += l.TotalTokens
	c.bucket.CacheReadTokens += l.CacheReadTokens
	c.bucket.CacheCreationTokens += l.CacheCreationTokens
	c.bucket.Cost += l.Cost
	c.bucket.TotalLatencyMS += l.LatencyMS
}

// bucketKeyOf 从统计桶构造唯一键（与 aggregate/refill 同一类型，保证回灌合并
// 到原桶）。
func bucketKeyOf(b *domain.StatBucket) statBucketKey {
	return statBucketKeyOf(b.BucketTime.Unix(), b.GroupID, b.AccountID, b.TemplateID, b.UserID, b.Model, b.IsError)
}

func statBucketKeyOf(hourUnix, groupID, accountID, templateID, userID int64, model string, isErr bool) statBucketKey {
	return statBucketKey{
		hourUnix: hourUnix, groupID: groupID, accountID: accountID,
		templateID: templateID, userID: userID, model: model, isErr: isErr,
	}
}

// shardFor 分片索引：同 key 恒同 worker（FNV-1a 哈希取模；确定性，测试断言
// swap/分片一致性用）。零分配（内联 FNV-1a；flush 期调用，性能非关键）。
func shardFor(key statBucketKey, workers int) int {
	const (
		offset32 = uint32(2166136261)
		prime32  = uint32(16777619)
	)
	h := offset32
	var b [8]byte
	step := func(v int64) {
		putUint64LE(b[:], uint64(v))
		for _, c := range b {
			h ^= uint32(c)
			h *= prime32
		}
	}
	step(key.hourUnix)
	step(key.groupID)
	step(key.accountID)
	step(key.templateID)
	step(key.userID)
	for i := 0; i < len(key.model); i++ {
		h ^= uint32(key.model[i])
		h *= prime32
	}
	if key.isErr {
		h ^= 1
		h *= prime32
	}
	return int(h % uint32(workers))
}

func putUint64LE(b []byte, v uint64) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
}

func (r *Recorder) logWriterLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// 最终排空由 Close 以 shutdown 预算 ctx 执行（O2 停机纪律）——本
			// loop ctx 在 SIGTERM 即已取消，此处 flush 传它会恒截断丢全部明细；
			// Close 持预算 ctx 才能"正常完整刷 / 到期截断"两全。
			return
		case <-t.C:
			r.flushLogs(r.baseCtx)
		}
	}
}

// flushLogs 换批 + 并行落库（O1 管道化消费侧）：锁内 swap 整个 pending（换新
// slice，flush 期间新日志进新 pending 零阻塞）→ 按 userID 分片（同 user 恒同
// worker）→ N worker 并发逐 chunk InsertBatch（chunk = cfg.BatchSize；ent
// CreateBulk 参数上限 PG 65535，500 × ~20 列安全）→ 失败 chunk 连同其后剩余
// 一并回灌 pending（不丢，下次 flush 重试；DB 故障不锤击——本 shard 停止；
// 连续失败 ≥ maxLogFlushFailures 显式止损丢弃，见下）。预算到期：未处理部分
// 原样回灌（由 Close 决定截断放弃）。返回本批成功落库条数（Close 汇总作
// Warn 诊断）。flushMu 串行单入口（ticker/Close 两处触发共用；在途批次即其
// 持有者——Close 以获取 flushMu 等待在途批次）。**互斥耦合（评审 I-1）**：
// flushLogs 与 flushStats 共用 flushMu（Close 在途屏障需要）——DB 故障积压
// 时单次 flushLogs 占锁可致额度回写/统计 Upsert 延迟同幅放大（非丢数据）。
// 毒丸行止损（评审 I-3）：单 chunk 连续失败 ≥ maxLogFlushFailures → Error
// 日志（含首行 request_id）+ 显式丢弃该 chunk（不再无限回灌卡死本 shard；
// 显式止损，非静默丢）。
func (r *Recorder) flushLogs(ctx context.Context) int64 {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()

	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return 0
	}
	pend := r.pending
	r.pending = nil
	r.pendingN.Add(-int64(len(pend)))
	if r.pendingN.Load() < pendingWaterline {
		r.warned.Store(false)
	}
	r.mu.Unlock()

	if ctx.Err() != nil { // 预算到期：不落库，原样回灌（Close 截断路径决定放弃）
		r.refillLogs(pend)
		return 0
	}

	shards := make([][]*domain.UsageLog, r.workers)
	for i := range shards {
		shards[i] = make([]*domain.UsageLog, 0, len(pend)/r.workers+1)
	}
	for _, l := range pend {
		shards[uint64(l.UserID)%uint64(r.workers)] = append(shards[uint64(l.UserID)%uint64(r.workers)], l)
	}

	var wg sync.WaitGroup
	var drained atomic.Int64
	for si, shard := range shards {
		if len(shard) == 0 {
			continue
		}
		wg.Add(1)
		go func(si int, s []*domain.UsageLog) {
			defer wg.Done()
			for start := 0; start < len(s); start += r.cfg.BatchSize {
				if ctx.Err() != nil { // 预算到期：剩余回灌
					r.refillLogs(s[start:])
					return
				}
				end := min(start+r.cfg.BatchSize, len(s))
				if err := r.logs.InsertBatch(ctx, s[start:end]); err != nil {
					r.failCounts[si]++ // 连续失败计数（仅失败路径写；热路径零触碰）
					if r.failCounts[si] >= maxLogFlushFailures {
						// 毒丸行止损（评审 I-3）：连续失败 ≥N 次 → 显式丢弃该
						// chunk（Error + 首行 request_id），隔离后不再回灌——
						// 避免单行永久失败（created_at 无分区/约束冲突）无限
						// 卡死本 shard（旧实现 workers=1 时整管道吞吐归零）。
						if r.log != nil {
							r.log.Error("usage batch insert failed, dropping poison chunk",
								logx.Error(err), logx.String("request_id", s[start].RequestID),
								logx.Int("dropped_logs", end-start))
						}
						r.failCounts[si] = 0
						r.refillLogs(s[end:]) // 毒丸 chunk 隔离丢弃；其后剩余回灌（不丢）
						return
					}
					if r.log != nil {
						r.log.Warn("usage batch insert failed", logx.Error(err))
					}
					r.refillLogs(s[start:]) // 失败 chunk + 其后剩余一并回灌（不丢）
					return
				}
				if r.failCounts[si] > 0 {
					r.failCounts[si] = 0 // 成功推进复位（仅对曾失败的 shard 写）
				}
				drained.Add(int64(end - start))
			}
		}(si, shard)
	}
	wg.Wait()
	return drained.Load()
}

// refillLogs 失败/截断回灌：合并回当前 pending（锁内 append——flush 期间
// Record 进新 slice，回灌与 Record 并发安全）。
func (r *Recorder) refillLogs(logs []*domain.UsageLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, logs...)
	r.pendingN.Add(int64(len(logs)))
}

func (r *Recorder) statsFlushLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.StatsFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// 最终 flush 由 Close 以 shutdown 预算 ctx 执行（O2 停机修复）：
			// 本 loop ctx 在 SIGTERM 即已取消，传它会恒截断丢全部统计（每
			// 次优雅停机都丢最后窗口，比不复用更糟）；Close 持预算 ctx 才能
			// "正常完整刷 / 到期截断"两全。跳过也消除与 Close 并发抢换批的
			// 竞态（谁先 swap 谁独占数据，后者见空）。
			return
		case <-t.C:
			r.flushStats(r.baseCtx)
		}
	}
}

// isDeadlockError 判断 PG 事务死锁（多实例并发批量 Upsert 同批 bucket 的
// DO UPDATE 锁顺序交错 → SQLSTATE 40P01；ent Conn.Exec 原样透传 pgconn 错误，
// 与 partition.go isDuplicateObject 同先例）。死锁是瞬时的：PG 回滚对端事务
// 后锁即释放——适合短退避重试消化，无需走回灌。
func isDeadlockError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40P01"
}

// deadlockRetryBackoff 死锁重试退避：10-50ms 区间均匀随机 jitter（math/rand/v2
// 全局并发安全）。多实例同时死锁失败后若同步等长重试，锁顺序依旧交错、再次
// 死锁；随机错开重试时刻，先到者持锁完成、后到者不再互斥——瞬时死锁被消化。
func deadlockRetryBackoff() time.Duration {
	return 10*time.Millisecond + time.Duration(rand.Int64N(int64(40*time.Millisecond)))
}

// flushStats 换批 + 落库（统计桶批量 Upsert + 额度增量批量回写），受 ctx 预算
// 约束（O2 停机修复——O1 复测：Close 用 Background 逐 key AddQuotaUsed 独占
// 3.8 分钟吃掉停机预算尾部，main 卡死）：
//   - 额度：按 quotaBatchSize 分组批量回写（单组一条 raw SQL CASE 更新——10k
//     逐 key 轮询是 #15 验收统计面慢 flush 3-5min 周期根因之一）；逐组前查
//     ctx.Err()，到期 → 截断（丢弃，崩溃等价语义）+ Warn（已刷/剩余组键数）；
//     失败整组回灌合并（下次 flush 重试，不丢）。
//   - 统计桶：按 bucket key 哈希分片（同桶恒同 worker）→ N worker 并发逐
//     chunk（statBatchSize）单条批量 Upsert（合并同桶冲突累加）；失败 chunk
//     回灌合并（不丢）；到期截断 + Warn。
// 截断丢的是统计面 stat 聚合/配额刷新（内存权威、DB 滞后 ≤ flush 间隔的崩溃
// 等价语义），**非计费扣费**——cost 经 billing Flusher 落库，与本统计面同窗口、
// 互不影响（billing_e2e 场景 9 优雅停机断流式 cost 不丢验证不受影响）。正常
// （无 deadline ctx）完整刷语义不变。
func (r *Recorder) flushStats(ctx context.Context) {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()

	r.mu.Lock()
	buckets := make([]*domain.StatBucket, 0, len(r.counters))
	for _, c := range r.counters {
		b := c.bucket
		buckets = append(buckets, &b)
	}
	r.counters = make(map[statBucketKey]*statCounters)
	quota := r.quotaUsed
	r.quotaUsed = make(map[int64]int64)
	qw := r.quota
	r.mu.Unlock()

	// 额度回写（增量；失败整组回灌合并，下次 flush 重试——与 stats 同语义）。
	if qw != nil && len(quota) > 0 {
		var flushedKeys, remainingKeys int
		keys := make([]int64, 0, len(quota))
		for k, v := range quota {
			if v != 0 { // 与 repo 语义一致：零增量无回写价值
				keys = append(keys, k)
			}
		}
		for start := 0; start < len(keys); start += quotaBatchSize {
			end := min(start+quotaBatchSize, len(keys))
			group := make(map[int64]int64, end-start)
			for _, k := range keys[start:end] {
				group[k] = quota[k]
			}
			if ctx.Err() != nil { // 预算到期：截断（丢弃，不落库不回灌）
				remainingKeys += len(group)
				continue
			}
			if err := qw.AddQuotaUsed(ctx, group); err != nil {
				if r.log != nil {
					r.log.Warn("usage quota writeback failed", logx.Error(err))
				}
				r.mu.Lock()
				for k, v := range group {
					r.quotaUsed[k] += v
				}
				r.mu.Unlock()
				continue
			}
			flushedKeys += len(group)
		}
		if remainingKeys > 0 && r.log != nil {
			r.log.Warn("usage quota flush truncated on shutdown budget",
				logx.Int("quota_flushed_keys", flushedKeys), logx.Int("quota_remaining_keys", remainingKeys))
		}
	}
	if len(buckets) == 0 {
		return
	}
	if ctx.Err() != nil { // 预算到期：截断（统计面聚合，崩溃等价语义）
		if r.log != nil {
			r.log.Warn("usage stats flush truncated on shutdown budget",
				logx.Int("stats_flushed_buckets", 0), logx.Int("stats_remaining_buckets", len(buckets)))
		}
		return
	}
	// 统计桶批量 Upsert：按 bucket key 哈希分片（同桶恒同 worker）→ 每 worker
	// 逐 chunk 单条 bulk upsert；失败 chunk 连同其后剩余回灌合并（不丢）。
	shards := make([][]*domain.StatBucket, r.workers)
	for i := range shards {
		shards[i] = make([]*domain.StatBucket, 0, len(buckets)/r.workers+1)
	}
	for _, b := range buckets {
		k := bucketKeyOf(b)
		sh := shardFor(k, r.workers) // I-6：局部变量，避免 shardFor 双次哈希
		shards[sh] = append(shards[sh], b)
	}

	var wg sync.WaitGroup
	var flushedB, remainingB atomic.Int64
	for _, shard := range shards {
		if len(shard) == 0 {
			continue
		}
		wg.Add(1)
		go func(s []*domain.StatBucket) {
			defer wg.Done()
			for start := 0; start < len(s); start += statBatchSize {
				if ctx.Err() != nil { // 预算到期：截断（丢弃，崩溃等价语义）
					remainingB.Add(int64(len(s) - start))
					return
				}
				end := min(start+statBatchSize, len(s))
				if err := r.stats.Upsert(ctx, s[start:end]); err != nil {
					// 死锁专属重试（#37 P3）：多实例并发批量 Upsert 同批 bucket
					// （DO UPDATE）锁顺序交错 → PG 事务死锁 40P01。死锁瞬时（对端
					// 回滚即释放锁）——短退避重试消化（≤2 次），避免反复 deadlock
					// 造成的统计延迟累积 + Warn 噪音；jitter 防两实例同步重试再
					// 次交错死锁。ctx 预算到期不重试（停机纪律，走现状回灌）。
					// 非 40P01 错误语义不变（直接回灌）。
					if isDeadlockError(err) {
						for attempt := 1; attempt <= maxStatsDeadlockRetries && ctx.Err() == nil; attempt++ {
							time.Sleep(deadlockRetryBackoff())
							if err = r.stats.Upsert(ctx, s[start:end]); err == nil || !isDeadlockError(err) {
								break
							}
						}
					}
					if err != nil {
						if r.log != nil {
							r.log.Warn("usage stats upsert failed", logx.Error(err))
						}
						r.refillBuckets(s[start:]) // 失败 chunk + 其后剩余回灌（不丢）
						return
					}
				}
				flushedB.Add(int64(end - start))
			}
		}(shard)
	}
	wg.Wait()
	if remainingB.Load() > 0 && r.log != nil {
		r.log.Warn("usage stats flush truncated on shutdown budget",
			logx.Int64("stats_flushed_buckets", flushedB.Load()),
			logx.Int64("stats_remaining_buckets", remainingB.Load()))
	}
}

// refillBuckets 失败回灌：合并回当前 counters（锁内——flush 期间 Record 聚合
// 进新 map，回灌与聚合并发安全；同 key 累加防重复计数）。
func (r *Recorder) refillBuckets(buckets []*domain.StatBucket) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range buckets {
		key := bucketKeyOf(b)
		if c, ok := r.counters[key]; ok {
			c.bucket.RequestCount += b.RequestCount
			c.bucket.ErrorCount += b.ErrorCount
			c.bucket.InputTokens += b.InputTokens
			c.bucket.OutputTokens += b.OutputTokens
			c.bucket.TotalTokens += b.TotalTokens
			c.bucket.CacheReadTokens += b.CacheReadTokens
			c.bucket.CacheCreationTokens += b.CacheCreationTokens
			c.bucket.Cost += b.Cost
			c.bucket.TotalLatencyMS += b.TotalLatencyMS
		} else {
			r.counters[key] = &statCounters{bucket: *b}
		}
	}
}

// Close 幂等排空（优雅停机核心）：等聚合 goroutine 退出（受预算约束）→ 以
// flushMu 获取等待在途批次（SIGTERM 时 ticker 批次可能已在途占住 flushMu 且
// pending 已 swap；Close 必须先等其结束，否则 drain 循环见 pendingN==0 会
// 静默提前返回，在途批次无界运行——O1 复测根因 1）→ 受 shutdown ctx 预算
// 约束的排空循环（此时无在途批次、flushMu 无竞争）。正常情形完整排空语义
// 不变（无 deadline ctx = 全部落库）；ctx 到期 → Cancel baseCtx（在途落库
// 快速失败回灌，不丢）+ Warn（flushed/remaining 条数单位一致）+ 截断退出，
// 不阻塞停机。统计面由 flushStats 以本 ctx 预算收尾（到期截断 + Warn）。
// 未 Start 也安全（跳过聚合等待；在途 flush 与 pending 残留同样等待/排空）。
func (r *Recorder) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		defer r.baseCancel() // recorder 关闭后 baseCtx 不得再有存活批次
		if r.startOnce.Load() {
			// 等聚合 goroutine 退出（受预算约束）。SIGTERM 时 loop 可能阻塞在
			// ticker flush（baseCtx 批次在途）——loopDone 待其批次结束 + 末次
			// flush 后才关闭；预算到期 → Warn + 继续（在途批次由下面 flushMu
			// 等待强制取消）。
			select {
			case <-r.loopDone:
			case <-ctx.Done():
				if r.log != nil {
					r.log.Warn("usage close: loops did not exit in time")
				}
			}
		}
		// 等在途批次（有界）：flushLogs/flushStats 由 flushMu 串行——"是否有
		// 批次在途"即"flushMu 是否被占"；尝试获取 flushMu：拿到即无在途批次
		// （其退出前必释放），预算内等其自然完成（完整排空语义不变）；到期 →
		// Cancel baseCtx 强制在途落库快速失败（回灌不丢），等批次收尾后走截
		// 断路径。未 Start 时无竞争立即拿到（此前测试直接调 flush 的在途批次
		// 同样被等待）。
		acquired := make(chan struct{})
		go func() { r.flushMu.Lock(); close(acquired) }()
		select {
		case <-acquired:
			r.flushMu.Unlock()
		case <-ctx.Done():
			r.baseCancel()
			<-acquired // 取消后在途批次快速收尾（落库尊重 ctx）
			r.flushMu.Unlock()
		}
		// 排空明细（预算内循环；到期 → Warn 截断退出——flushed/remaining 均
		// 为明细条数，单位一致）。
		var flushed int64
		for r.pendingN.Load() > 0 {
			if ctx.Err() != nil { // 预算到期：截断退出（剩余明细由 flushLogs 截断回灌，丢 ≤1 flush 窗口）
				if r.log != nil {
					r.log.Warn("usage close: shutdown budget exceeded, truncated drain",
						logx.Int64("flushed_logs", flushed), logx.Int64("remaining_logs", r.pendingN.Load()))
				}
				break
			}
			flushed += r.flushLogs(ctx)
		}
		// 统计面收尾：flushStats 内部受 ctx 预算约束（到期 → 截断 Warn，崩溃
		// 等价语义；正常完整刷）。预算已到期时此处即"统计截断"告警面。
		r.flushStats(ctx)
		// Close 完成后置位 closed（评审 I-4）：后续 Record 走 Warn 一次路径
		//（明细仍聚合入 pending 不丢，驻留内存由 Warn 观测）。
		r.closed.Store(true)
	})
	return nil
}
