// Package usage 承载请求明细的异步落库与预聚合统计（规格 §7.2/§10.5）以及
// usagelog 保留策略（retention worker，Phase 5 T4.5：按日分区 DROP 清理）。
// 统计聚合永不失真（同步进内存计数），明细经有界 channel 批量落库、
// 饱和时阻塞反压（用户决策 2026-08-05：不得丢数据）。
package usage

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

type UsageConfig struct {
	BatchSize          int
	FlushInterval      time.Duration
	StatsFlushInterval time.Duration
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

type Recorder struct {
	cfg       UsageConfig
	logs      LogInserter
	stats     StatUpserter
	quota     QuotaWriter // 可选（nil = 不回写额度）
	log       *logx.Logger
	logCh     chan *domain.UsageLog
	mu        sync.Mutex
	counters  map[string]*statCounters
	quotaUsed map[int64]int64 // key_id → 待回写 token 增量
	startOnce atomic.Bool
}

type statCounters struct {
	bucket domain.StatBucket
}

func New(cfg UsageConfig, logs LogInserter, stats StatUpserter, log *logx.Logger) *Recorder {
	return &Recorder{
		cfg:       cfg,
		logs:      logs,
		stats:     stats,
		log:       log,
		logCh:     make(chan *domain.UsageLog, 16384),
		counters:  make(map[string]*statCounters),
		quotaUsed: make(map[int64]int64),
	}
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
	go r.logWriterLoop(ctx)
	go r.statsFlushLoop(ctx)
	return nil
}

// Record 记录一次请求：统计同步聚合（永不丢弃），明细入有界 channel。
// 常时非阻塞；channel 饱和时阻塞反压（用户决策 2026-08-05：不得丢数据），
// 反压传导至请求路径，由 HTTP 层过载保护（max_inflight，规格 §10.6）兜底。
func (r *Recorder) Record(l *domain.UsageLog) {
	r.Aggregate(l)
	r.logCh <- l
}

// Aggregate 同步聚合统计（请求数/错误/tokens/cost 进 StatBucket，不入明细
// channel）——T3 计费 Flusher 复用同一聚合（billed 请求只经 Flusher 不落本
// 明细 channel；每日志恰好一个写者）。与 Record 等价，仅跳过明细投递。
func (r *Recorder) Aggregate(l *domain.UsageLog) {
	r.aggregate(l)
}

// Pending 返回尚未落库的明细条数（测试与背压观测用）。
func (r *Recorder) Pending() int { return len(r.logCh) }

func (r *Recorder) aggregate(l *domain.UsageLog) {
	hour := l.CreatedAt.UTC().Truncate(time.Hour)
	isErr := l.ErrorType != domain.ErrNone
	key := fmt.Sprintf("%d|%d|%d|%d|%d|%s|%v", hour.Unix(), l.GroupID, l.AccountID, l.TemplateID, l.UserID, l.Model, isErr)
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
	c.bucket.PromptTokens += l.PromptTokens
	c.bucket.CompletionTokens += l.CompletionTokens
	c.bucket.TotalTokens += l.TotalTokens
	c.bucket.CacheReadTokens += l.CacheReadTokens
	c.bucket.CacheCreationTokens += l.CacheCreationTokens
	c.bucket.Cost += l.Cost
	c.bucket.TotalLatencyMS += l.LatencyMS
}

func (r *Recorder) logWriterLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.FlushInterval)
	defer t.Stop()
	batch := make([]*domain.UsageLog, 0, r.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.logs.InsertBatch(context.Background(), batch); err != nil {
			if r.log != nil {
				r.log.Warn("usage batch insert failed", logx.Error(err))
			}
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case <-t.C:
			flush()
		case l := <-r.logCh:
			batch = append(batch, l)
			if len(batch) >= r.cfg.BatchSize {
				flush()
			}
		}
	}
}

func (r *Recorder) statsFlushLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.StatsFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.flushStats()
			return
		case <-t.C:
			r.flushStats()
		}
	}
}

func (r *Recorder) flushStats() {
	r.mu.Lock()
	buckets := make([]*domain.StatBucket, 0, len(r.counters))
	for _, c := range r.counters {
		b := c.bucket
		buckets = append(buckets, &b)
	}
	r.counters = make(map[string]*statCounters)
	quota := r.quotaUsed
	r.quotaUsed = make(map[int64]int64)
	qw := r.quota
	r.mu.Unlock()
	// 额度回写（增量；失败回灌，下次 flush 重试——与 stats 同语义）
	if qw != nil && len(quota) > 0 {
		if err := qw.AddQuotaUsed(context.Background(), quota); err != nil {
			if r.log != nil {
				r.log.Warn("usage quota writeback failed", logx.Error(err))
			}
			r.mu.Lock()
			for k, v := range quota {
				r.quotaUsed[k] += v
			}
			r.mu.Unlock()
		}
	}
	if len(buckets) == 0 {
		return
	}
	if err := r.stats.Upsert(context.Background(), buckets); err != nil {
		if r.log != nil {
			r.log.Warn("usage stats upsert failed", logx.Error(err))
		}
		// 失败回灌：避免计数丢失
		r.mu.Lock()
		for _, b := range buckets {
			key := fmt.Sprintf("%d|%d|%d|%d|%d|%s|%v", b.BucketTime.Unix(), b.GroupID, b.AccountID, b.TemplateID, b.UserID, b.Model, b.IsError)
			if c, ok := r.counters[key]; ok {
				c.bucket.RequestCount += b.RequestCount
				c.bucket.ErrorCount += b.ErrorCount
				c.bucket.PromptTokens += b.PromptTokens
				c.bucket.CompletionTokens += b.CompletionTokens
				c.bucket.TotalTokens += b.TotalTokens
				c.bucket.CacheReadTokens += b.CacheReadTokens
				c.bucket.CacheCreationTokens += b.CacheCreationTokens
				c.bucket.Cost += b.Cost
				c.bucket.TotalLatencyMS += b.TotalLatencyMS
			} else {
				r.counters[key] = &statCounters{bucket: *b}
			}
		}
		r.mu.Unlock()
	}
}

// Close 排空剩余明细（限时，超时丢弃并 Warn）；幂等，满足 worker.Worker 契约。
func (r *Recorder) Close(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case l := <-r.logCh:
				if err := r.logs.InsertBatch(context.Background(), []*domain.UsageLog{l}); err != nil && r.log != nil {
					r.log.Warn("usage final flush failed", logx.Error(err))
				}
			default:
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		if r.log != nil {
			r.log.Warn("usage close timeout, dropping remaining logs")
		}
	}
	r.flushStats()
	return nil
}
