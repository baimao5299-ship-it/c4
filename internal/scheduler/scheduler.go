// Package scheduler 实现内存优先的账号调度：规则驱动的状态管理（internal/rule 引擎
// 事件投递 + apply 回调）、选号（格式硬过滤 + 模型偏好 + 预生成加权轮询序列）、
// 并发槽、快照缓存与异步状态回写。规格 §5。单实例语义：运行时状态仅存内存。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/rule"
	"go-proxy-mini/pkg/logx"
)

var (
	ErrGroupNotFound     = errors.New("scheduler: group not found")
	ErrFormatUnavailable = errors.New("scheduler: no account for request format")
	ErrNoAvailable       = errors.New("scheduler: no available account")
)

type Config struct {
	DefaultMaxConcurrency int
	SyncInterval          time.Duration
}

// Loader 是调度器的数据源（由 repository 实现）。
type Loader interface {
	LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error)
	LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error)
	UpdateAccountStatus(ctx context.Context, accountID int64, status domain.AccountStatus, cooldownUntil *time.Time, lastError *string, weight *int) error
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
	weight   *int // 权重动作随状态同批回写（nil = 不动 weight）
}

type Scheduler struct {
	cfg       Config
	loader    Loader
	rule      *rule.RuleEngine
	log       *logx.Logger
	store     snapshotStore
	reloadMu  sync.Mutex // 重建互斥（低频，不占热路径）
	writeCh   chan statusWrite
	timeNow   func() time.Time
	startOnce atomic.Bool
}

// New 构造调度器并注册规则引擎的 apply 回调（动作应用 = 快照/EWMA/回写，见 apply）。
// ruleEngine 必须非 nil（状态管理唯一路径；main 在 Start 前显式 Reload）。
func New(cfg Config, loader Loader, ruleEngine *rule.RuleEngine, log *logx.Logger) *Scheduler {
	s := &Scheduler{
		cfg:     cfg,
		loader:  loader,
		rule:    ruleEngine,
		log:     log,
		writeCh: make(chan statusWrite, 4096),
		timeNow: time.Now,
	}
	ruleEngine.SetApply(s.apply)
	return s
}

// Name 满足 worker.Worker 契约（Global Constraints #5）。
func (s *Scheduler) Name() string { return "scheduler" }

// Start 启动定时同步与异步状态回写；重复 Start 幂等（返回错误）。
func (s *Scheduler) Start(ctx context.Context) error {
	if !s.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("scheduler: already started")
	}
	go s.syncLoop(ctx)
	go s.writebackLoop(ctx)
	return nil
}

// Close 排空剩余状态回写（限时，复用 writebackLoop 的合并逻辑）；幂等，
// 满足 worker.Worker 契约。循环本身随 Start 的 ctx 取消而退出。
func (s *Scheduler) Close(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case w := <-s.writeCh:
				s.processWrite(w)
			default:
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		if s.log != nil {
			s.log.Warn("scheduler close timeout, dropping pending writebacks")
		}
	}
	return nil
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
			s.processWrite(w)
		}
	}
}

// processWrite 处理一条状态回写：合并窗口内同一账号的重复写（幂等覆盖）后回写 DB。
// 合并语义：后写覆盖先写，但 weight 例外——后写若不带 weight（statusWrite.weight=nil，
// 纯状态动作），保留先前已入队的 weight（否则同账号 weight 写先入队、status 写后
// 入队时合并丢 weight，DB 不持久化 → ≤30s reload 后内存回退，weight 动作被静默撤销）。
func (s *Scheduler) processWrite(w statusWrite) {
	accs := map[int64]statusWrite{w.id: w}
	drain := true
	for drain {
		select {
		case w2 := <-s.writeCh:
			if prev, ok := accs[w2.id]; ok && w2.weight == nil && prev.weight != nil {
				w2.weight = prev.weight
			}
			accs[w2.id] = w2
		default:
			drain = false
		}
	}
	for _, ww := range accs {
		if err := s.loader.UpdateAccountStatus(context.Background(), ww.id, ww.status, ww.cooldown, ww.lastErr, ww.weight); err != nil && s.log != nil {
			s.log.Warn("account status writeback failed", logx.Int64("account_id", ww.id), logx.Error(err))
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
			as := &accountSnapshot{gid: gid, acc: *a, tpl: a.Template}
			as.state.Store(&accState{status: a.Status, cooldownUntil: a.CooldownUntil})
			if a.MaxConcurrency <= 0 {
				as.acc.MaxConcurrency = defaultMax
			}
			gs.accounts = append(gs.accounts, as)
			byID[a.ID] = as
		}
		gs.routes = buildRoutes(gs.accounts)
		groups[gid] = gs
	}
	return groups, byID
}

// modelSet 组内所有账号模板的可服务模型并集（桶 key 的模型空间）。
func modelSet(accs []*accountSnapshot) map[string]struct{} {
	set := make(map[string]struct{})
	for _, a := range accs {
		if a.tpl == nil {
			continue
		}
		for _, m := range a.tpl.Models {
			set[m] = struct{}{}
		}
		for _, list := range a.tpl.FormatModels {
			for _, m := range list {
				set[m] = struct{}{}
			}
		}
		for m := range a.tpl.ModelMapping {
			set[m] = struct{}{}
		}
	}
	return set
}

// buildRoutes 预生成 (format, model) 调度路径：格式硬过滤（FormatSupports）与模型偏好
// （Serves）都是静态信息，可完全在重建时计算。另为每个格式生成默认回退桶
// （model == ""）：请求模型未知时行为等价于格式桶 + tier2（Serves 恒 false）。
func buildRoutes(accs []*accountSnapshot) map[routeKey]*route {
	routes := make(map[routeKey]*route)
	formats := []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses, domain.FormatAnthropic}
	for model := range modelSet(accs) {
		for _, format := range formats {
			var t1, t2 []*accountSnapshot
			for _, a := range accs {
				if a.tpl == nil || !a.tpl.FormatSupports(format, model) {
					continue
				}
				if a.tpl.Serves(model) {
					t1 = append(t1, a)
				} else {
					t2 = append(t2, a)
				}
			}
			if len(t1) == 0 && len(t2) == 0 {
				continue
			}
			rt := &route{}
			if len(t1) > 0 {
				rt.tier1 = newWeightedSeq(t1)
			}
			if len(t2) > 0 {
				rt.tier2 = newWeightedSeq(t2)
			}
			routes[routeKey{format, model}] = rt
		}
	}
	for _, format := range formats {
		var t2 []*accountSnapshot
		for _, a := range accs {
			if a.tpl == nil || !slices.Contains(a.tpl.SupportedFormats, format) {
				continue
			}
			t2 = append(t2, a)
		}
		if len(t2) == 0 {
			continue
		}
		routes[routeKey{format, ""}] = &route{tier2: newWeightedSeq(t2)}
	}
	return routes
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
	newAccs := gs[groupID].accounts
	// 直接复用 buildSnapshots 产出的快照：accounts 与 routes 一并生效，
	// 避免组级重载后 routes 为 nil（Select 预生成路径断裂）。
	newM[groupID] = gs[groupID]
	// byID 必须与 groups 同步重建（评审发现：只换 groups 会导致并发计数/结果回写
	// 落到旧快照——Select 计数在新快照、Release/MarkResult 查 byID 命中旧快照，
	// 计数只增不减直至全量 reload；新账号的回写被静默丢弃）。
	newByID := make(map[int64]*accountSnapshot, len(byID)+len(newAccs))
	for k, v := range byID {
		newByID[k] = v
	}
	if old, ok := m[groupID]; ok {
		for _, os := range old.accounts {
			stillIn := false
			for _, ns := range newAccs {
				if ns.acc.ID == os.acc.ID {
					stillIn = true
					break
				}
			}
			if !stillIn {
				delete(newByID, os.acc.ID)
			}
		}
	}
	for _, ns := range newAccs {
		newByID[ns.acc.ID] = ns
	}
	s.store.store(newM, newByID)
}

func (s *Scheduler) InvalidateAll() {
	if err := s.reload(context.Background()); err != nil && s.log != nil {
		s.log.Warn("scheduler reload failed", logx.Error(err))
	}
}

// Loader 暴露数据源（测试注入用）。
func (s *Scheduler) Loader() Loader { return s.loader }

// InvalidateAllSync 同步全量重载（测试与启动用）。
func (s *Scheduler) InvalidateAllSync() error { return s.reload(context.Background()) }

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

// MarkResult 请求结果回流：禁用守卫（同步短路）+ 条件投递（C1）→ 规则引擎异步处理。
// 快照/EWMA/组路由/DB 回写全部由规则命中后的 apply 回调完成（本方法不再触碰状态）。
func (s *Scheduler) MarkResult(accountID int64, kind ResultKind, resetAt *time.Time, httpStatus int, errMsg string) {
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	a, ok := byID[accountID]
	if !ok {
		return
	}
	// 禁用账号的防复活守卫：管理端禁用后（InvalidateGroup 以 disabled 重载
	// 快照），在途请求完成时不得投递事件把状态重置回 active 并回写 DB——否则
	// 禁用被静默抹除、30s 同步后账号复现（评审发现）。禁用账号不参与选号，
	// err/429 分支同样不可能合法触发于其上，统一在此短路（不投递）。
	if a.statePtr().status == domain.StatusDisabled {
		return
	}
	// 条件投递（C1）：规则表无 kind=nil/ok 规则时 ok 事件不投递
	// （无恢复规则时成功结果不影响任何状态，省队列与处理开销）。
	if kind == ResultOK && !s.rule.NeedsOKEvents() {
		return
	}
	var hp *int
	if httpStatus > 0 {
		hp = &httpStatus
	}
	ev := rule.Event{
		AccountID:    accountID,
		TemplateID:   a.acc.TemplateID,
		GroupID:      groupIDPtr(a.gid),
		Kind:         ruleKind(kind),
		HTTPStatus:   hp,
		ErrorMessage: errMsg,
		ResetAt:      resetAt,
		OccurredAt:   s.timeNow(),
	}
	s.rule.Enqueue(ev)
}

// ruleKind 映射 ResultKind → 规则引擎事件类别。
func ruleKind(k ResultKind) rule.Kind {
	switch k {
	case Result429:
		return rule.Kind429
	case ResultError:
		return rule.KindError
	default:
		return rule.KindOK
	}
}

func groupIDPtr(gid int64) *int64 {
	if gid <= 0 {
		return nil
	}
	return &gid
}

func strPtr(s string) *string { return &s }

// FlushRules 同步处理规则引擎队列中的全部事件（仅测试与优雅关闭用）：
// MarkResult 为异步投递，需要立即断言快照的测试先排空队列。
func (s *Scheduler) FlushRules() {
	s.rule.Flush(context.Background())
}

// apply 是规则引擎的动作应用回调（New 时注册）：更新快照状态/冷却/权重、
// EWMA（仅状态类动作）、权重变更时重建组路由（weightedSeq 预生成缓存）、
// 异步 DB 回写。st 为 nil = 只改权重/冷却，不动状态与 EWMA。
func (s *Scheduler) apply(aid int64, st *domain.AccountStatus, cooldownUntil *time.Time, weight *int) {
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	a, ok := byID[aid]
	if !ok {
		return // 快照外账号（已移除/未知）：无状态可改，不投递回写
	}
	now := s.timeNow()
	next := *a.statePtr()
	if st != nil {
		next.status = *st
		switch *st {
		case domain.Status429:
			next.errCount++
			next.lastError = strPtr("upstream 429 rate limited")
		case domain.StatusUnhealthy:
			next.errCount++
			next.lastError = strPtr("upstream error")
		case domain.StatusActive:
			next.errCount = 0
			next.lastError = nil
		}
		// EWMA：α=0.2；仅状态类动作更新（ok=0、429/error=1 的 rateDelta，
		// 纯 weight 动作不更新——I5）
		rateDelta := 0.0
		if *st == domain.Status429 || *st == domain.StatusUnhealthy {
			rateDelta = 1
		}
		old := float64(a.errRate.Load()) / errRateScale
		rate := 0.2*rateDelta + 0.8*old
		a.errRate.Store(uint64(rate * errRateScale))
	}
	if cooldownUntil != nil {
		next.cooldownUntil = cooldownUntil
	}
	next.lastUsedAt = &now
	a.state.Store(&next)
	if weight != nil {
		a.acc.Weight = *weight
		// weightedSeq 是预生成缓存：权重变更必须重建该组路由序列，
		// 否则选号仍按旧权重（I1）。
		s.rebuildGroup(a.gid)
	}
	s.enqueueWrite(aid, next, weight)
}

// rebuildGroup 重建单组路由（不碰 DB/账号列表）：从 store 中现有账号快照
// （apply 已更新 acc.Weight）重新 buildRoutes，整体换入快照（原子替换，避免
// 与 Select 读端并发修改同一 groupSnapshot 的数据竞争）。byID 不变（同一批
// accountSnapshot 指针）。
func (s *Scheduler) rebuildGroup(groupID int64) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	m := s.store.groups.Load().(map[int64]*groupSnapshot)
	gs, ok := m[groupID]
	if !ok {
		return
	}
	newM := make(map[int64]*groupSnapshot, len(m))
	for k, v := range m {
		newM[k] = v
	}
	newM[groupID] = &groupSnapshot{accounts: gs.accounts, routes: buildRoutes(gs.accounts)}
	s.store.store(newM, s.store.byID.Load().(map[int64]*accountSnapshot))
}

func (s *Scheduler) enqueueWrite(id int64, st accState, weight *int) {
	select {
	case s.writeCh <- statusWrite{id: id, status: st.status, cooldown: st.cooldownUntil, lastErr: st.lastError, weight: weight}:
	default:
		// 队列满：丢弃 DB 回写（内存状态已生效，重启后由下一次请求重新判定）
	}
}
