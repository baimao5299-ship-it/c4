// Package usage 承载请求明细的异步落库与预聚合统计（规格 §7.2/§10.5）。
// 统计聚合永不失真（同步进内存计数），明细经有界 channel 批量落库、饱和时可丢弃。
package usage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

type UsageConfig struct {
	BatchSize          int
	FlushInterval      time.Duration
	DropOnFull         bool
	LogRetentionDays   int
	StatsFlushInterval time.Duration
}

type LogInserter interface {
	InsertBatch(ctx context.Context, logs []*domain.UsageLog) error
}

type StatUpserter interface {
	Upsert(ctx context.Context, buckets []*domain.StatBucket) error
}

type Recorder struct {
	cfg      UsageConfig
	logs     LogInserter
	stats    StatUpserter
	log      *logx.Logger
	logCh    chan *domain.UsageLog
	mu       sync.Mutex
	counters map[string]*statCounters
	dropped  int64
}

type statCounters struct {
	bucket domain.StatBucket
}

func New(cfg UsageConfig, logs LogInserter, stats StatUpserter, log *logx.Logger) *Recorder {
	return &Recorder{
		cfg:      cfg,
		logs:     logs,
		stats:    stats,
		log:      log,
		logCh:    make(chan *domain.UsageLog, 16384),
		counters: make(map[string]*statCounters),
	}
}

func (r *Recorder) Start(ctx context.Context) {
	go r.logWriterLoop(ctx)
	go r.statsFlushLoop(ctx)
	go r.janitorLoop(ctx)
}

// Record 记录一次请求：统计同步聚合（永不丢弃），明细入有界 channel。
func (r *Recorder) Record(l *domain.UsageLog) {
	r.aggregate(l)
	if r.cfg.DropOnFull {
		select {
		case r.logCh <- l:
		default:
			r.dropped++
			if r.log != nil && r.dropped%1000 == 1 {
				r.log.Warn("usage log dropped (pipeline saturated)", logx.Int64("dropped", r.dropped))
			}
		}
		return
	}
	r.logCh <- l
}

func (r *Recorder) aggregate(l *domain.UsageLog) {
	hour := l.CreatedAt.UTC().Truncate(time.Hour)
	isErr := l.ErrorType != domain.ErrNone
	key := fmt.Sprintf("%d|%d|%d|%d|%s|%v", hour.Unix(), l.GroupID, l.AccountID, l.TemplateID, l.Model, isErr)
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[key]
	if !ok {
		c = &statCounters{bucket: domain.StatBucket{
			BucketTime: hour, GroupID: l.GroupID, AccountID: l.AccountID,
			TemplateID: l.TemplateID, Model: l.Model, IsError: isErr,
		}}
		r.counters[key] = c
	}
	c.bucket.RequestCount++
	if isErr {
		c.bucket.ErrorCount++
	}
	c.bucket.PromptTokens += l.PromptTokens
	c.bucket.CompletionTokens += l.CompletionTokens
	c.bucket.TotalTokens += l.TotalTokens
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
	r.mu.Unlock()
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
			key := fmt.Sprintf("%d|%d|%d|%d|%s|%v", b.BucketTime.Unix(), b.GroupID, b.AccountID, b.TemplateID, b.Model, b.IsError)
			if c, ok := r.counters[key]; ok {
				c.bucket.RequestCount += b.RequestCount
				c.bucket.ErrorCount += b.ErrorCount
				c.bucket.PromptTokens += b.PromptTokens
				c.bucket.CompletionTokens += b.CompletionTokens
				c.bucket.TotalTokens += b.TotalTokens
				c.bucket.TotalLatencyMS += b.TotalLatencyMS
			} else {
				r.counters[key] = &statCounters{bucket: *b}
			}
		}
		r.mu.Unlock()
	}
}

// Close 排空剩余明细（限时，超时丢弃并 Warn）。
func (r *Recorder) Close(ctx context.Context) {
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
}

func (r *Recorder) janitorLoop(ctx context.Context) {
	if r.cfg.LogRetentionDays <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.purgeLogs()
		}
	}
}

// purgeLogs 依赖 LogInserter 扩展接口（可选实现）。
func (r *Recorder) purgeLogs() {
	if p, ok := r.logs.(interface {
		PurgeLogs(ctx context.Context, olderThan time.Time) error
	}); ok {
		cutoff := time.Now().Add(-time.Duration(r.cfg.LogRetentionDays) * 24 * time.Hour)
		if err := p.PurgeLogs(context.Background(), cutoff); err != nil && r.log != nil {
			r.log.Warn("usage log purge failed", logx.Error(err))
		}
	}
}
