// Package scheduler 实现内存优先的账号调度：状态机（err/429 冷却 + 指数退避）、
// 选号（格式硬过滤 + 模型偏好 + 加权随机）、并发槽、快照缓存与异步状态回写。
// 规格 §5。单实例语义：运行时状态仅存内存。
package scheduler

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/logx"
)

var (
	ErrGroupNotFound     = errors.New("scheduler: group not found")
	ErrFormatUnavailable = errors.New("scheduler: no account for request format")
	ErrNoAvailable       = errors.New("scheduler: no available account")
)

type Config struct {
	DefaultMaxConcurrency int
	Cooldown429           time.Duration
	BackoffBase           time.Duration
	BackoffMax            time.Duration
	SyncInterval          time.Duration
}

// Loader 是调度器的数据源（由 repository 实现）。
type Loader interface {
	LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error)
	LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error)
	UpdateAccountStatus(ctx context.Context, accountID int64, status domain.AccountStatus, cooldownUntil *time.Time, lastError *string) error
}

type ResultKind int

const (
	ResultOK ResultKind = iota
	Result429
	ResultError
)

type Selection struct {
	AccountID   int64
	TemplateID  int64
	BaseURL     string
	Format      domain.RequestFormat
	UpstreamKey string
	Model       string // 已应用模型映射
}

type RuntimeInfo struct {
	Status        domain.AccountStatus
	CooldownUntil *time.Time
	Concurrency   int64
	ErrRate       float64
	ErrCount      int
}

type statusWrite struct {
	id       int64
	status   domain.AccountStatus
	cooldown *time.Time
	lastErr  *string
}

type Scheduler struct {
	cfg      Config
	loader   Loader
	log      *logx.Logger
	store    snapshotStore
	reloadMu sync.Mutex // 重建互斥（低频，不占热路径）
	writeCh  chan statusWrite
	timeNow  func() time.Time
}

func New(cfg Config, loader Loader, log *logx.Logger) *Scheduler {
	return &Scheduler{
		cfg:     cfg,
		loader:  loader,
		log:     log,
		writeCh: make(chan statusWrite, 4096),
		timeNow: time.Now,
	}
}

// Start 启动定时同步与异步状态回写。
func (s *Scheduler) Start(ctx context.Context) {
	go s.syncLoop(ctx)
	go s.writebackLoop(ctx)
}

func (s *Scheduler) syncLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.reload(ctx); err != nil && s.log != nil {
				s.log.Warn("scheduler sync failed", logx.Error(err))
			}
		}
	}
}

func (s *Scheduler) writebackLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case w := <-s.writeCh:
			// 合并窗口内同一账号的重复写（幂等覆盖）
			accs := map[int64]statusWrite{w.id: w}
			drain := true
			for drain {
				select {
				case w2 := <-s.writeCh:
					accs[w2.id] = w2
				default:
					drain = false
				}
			}
			for _, ww := range accs {
				if err := s.loader.UpdateAccountStatus(context.Background(), ww.id, ww.status, ww.cooldown, ww.lastErr); err != nil && s.log != nil {
					s.log.Warn("account status writeback failed", logx.Int64("account_id", ww.id), logx.Error(err))
				}
			}
		}
	}
}

// reload 全量重建快照（启动/定时/InvalidateAll）。
func (s *Scheduler) reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	m, err := s.loader.LoadGroupsAccounts(ctx)
	if err != nil {
		return err
	}
	s.store.store(buildSnapshots(m, s.cfg.DefaultMaxConcurrency))
	return nil
}

func buildSnapshots(m map[int64][]*domain.Account, defaultMax int) (map[int64]*groupSnapshot, map[int64]*accountSnapshot) {
	groups := make(map[int64]*groupSnapshot, len(m))
	byID := make(map[int64]*accountSnapshot)
	for gid, accs := range m {
		gs := &groupSnapshot{}
		for _, a := range accs {
			as := &accountSnapshot{acc: *a, tpl: a.Template}
			as.state.Store(&accState{status: a.Status, cooldownUntil: a.CooldownUntil})
			if a.MaxConcurrency <= 0 {
				as.acc.MaxConcurrency = defaultMax
			}
			gs.accounts = append(gs.accounts, as)
			byID[a.ID] = as
		}
		groups[gid] = gs
	}
	return groups, byID
}

func (s *Scheduler) InvalidateGroup(groupID int64) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	accs, err := s.loader.LoadGroupAccounts(context.Background(), groupID)
	if err != nil {
		if s.log != nil {
			s.log.Warn("group reload failed", logx.Int64("group_id", groupID), logx.Error(err))
		}
		return
	}
	m, byID := s.store.groups.Load().(map[int64]*groupSnapshot), s.store.byID.Load().(map[int64]*accountSnapshot)
	newM := make(map[int64]*groupSnapshot, len(m)+1)
	for k, v := range m {
		newM[k] = v
	}
	gs, _ := buildSnapshots(map[int64][]*domain.Account{groupID: accs}, s.cfg.DefaultMaxConcurrency)
	newM[groupID] = &groupSnapshot{accounts: gs[groupID].accounts}
	s.store.store(newM, byID)
}

func (s *Scheduler) InvalidateAll() {
	if err := s.reload(context.Background()); err != nil && s.log != nil {
		s.log.Warn("scheduler reload failed", logx.Error(err))
	}
}

// Runtime 供管理端展示运行时视图。
func (s *Scheduler) Runtime(accountID int64) (RuntimeInfo, bool) {
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	a, ok := byID[accountID]
	if !ok {
		return RuntimeInfo{}, false
	}
	st := a.statePtr()
	return RuntimeInfo{
		Status: st.status, CooldownUntil: st.cooldownUntil,
		Concurrency: a.concurrency.Load(),
		ErrRate:     float64(a.errRate.Load()) / errRateScale,
		ErrCount:    st.errCount,
	}, true
}

// Release 释放并发槽（请求结束必须调用，含流式断开）。
func (s *Scheduler) Release(accountID int64) {
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	if a, ok := byID[accountID]; ok {
		a.concurrency.Add(-1)
	}
}

// MarkResult 请求结果回流：更新状态/冷却/EWMA，异步回写 DB。
func (s *Scheduler) MarkResult(accountID int64, kind ResultKind, resetAt *time.Time) {
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	a, ok := byID[accountID]
	if !ok {
		return
	}
	now := s.timeNow()
	st := a.statePtr()
	var (
		next      accState
		cooldown  *time.Time
		lastErr   *string
		rateDelta float64
	)
	switch kind {
	case ResultOK:
		next = accState{status: domain.StatusActive, lastUsedAt: &now}
		rateDelta = 0
	case Result429:
		cooldown = resetAt
		if cooldown == nil {
			c := now.Add(s.cfg.Cooldown429)
			cooldown = &c
		}
		lastErr = strPtr("upstream 429 rate limited")
		next = accState{status: domain.Status429, cooldownUntil: cooldown, errCount: st.errCount + 1, lastError: lastErr, lastUsedAt: &now}
		rateDelta = 1
	case ResultError:
		backoff := backoffDuration(s.cfg.BackoffBase, s.cfg.BackoffMax, st.errCount)
		c := now.Add(backoff)
		cooldown = &c
		lastErr = strPtr("upstream error")
		next = accState{status: domain.StatusErr, cooldownUntil: cooldown, errCount: st.errCount + 1, lastError: lastErr, lastUsedAt: &now}
		rateDelta = 1
	}
	a.state.Store(&next)
	// EWMA：α=0.2
	old := float64(a.errRate.Load()) / errRateScale
	rate := 0.2*rateDelta + 0.8*old
	a.errRate.Store(uint64(rate * errRateScale))
	s.enqueueWrite(accountID, next)
}

func backoffDuration(base, max time.Duration, errCount int) time.Duration {
	d := time.Duration(float64(base) * math.Pow(2, float64(errCount)))
	if d > max {
		return max
	}
	return d
}

func strPtr(s string) *string { return &s }

func (s *Scheduler) enqueueWrite(id int64, st accState) {
	select {
	case s.writeCh <- statusWrite{id: id, status: st.status, cooldown: st.cooldownUntil, lastErr: st.lastError}:
	default:
		// 队列满：丢弃 DB 回写（内存状态已生效，重启后由下一次请求重新判定）
	}
}
