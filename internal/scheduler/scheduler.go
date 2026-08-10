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

	"go-proxy-mini/internal/credential"
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
	// GroupPub 状态回写成功后的组级 NOTIFY 发布器（#14 T3a：多实例传播——
	// 账号状态变更落库后广播受影响组，其余实例组级重载收敛分裂快照）。
	// 实现 = 装配侧 adapter（main 把 notify.Publisher 适配为
	// PublishGroups）；nil = 未装配（单实例/测试），no-op。
	GroupPub GroupChangePublisher
}

// GroupChangePublisher 组级 NOTIFY 发布面（设计文档 §1.3 / 必改 6）：
// apply 状态回写 DB 成功后发布受影响组 id——跨实例状态分裂的最大风险点
// （实例 A 禁号回写，实例 B 快照仍 active 继续选号）。计费/扣费路径不发布
// （scheduler 无扣费，全部是账号状态回写）。
type GroupChangePublisher interface {
	PublishGroups(ctx context.Context, gids []int64)
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
	AccountID      int64
	TemplateID     int64
	BaseURL        string
	Format         domain.RequestFormat
	UpstreamKey    string
	CredentialType credential.Type
	Model          string // 已应用模型映射
	// StripImageTools 模板级图像 tool 剥离开关快照（pickFrom 从模板快照复制；
	// 热路径布尔读 + 分支零开销；W4 消费）。
	StripImageTools bool
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
// 全部回写成功后发布一次组级 NOTIFY（#14 T3a）：合并本批受影响组（去重，防载荷
// 膨胀超 R9 上限）——一次回写批次一条 NOTIFY（R3）。快照外账号（已移除）跳过：
// 无组可传播，其余实例经 ≤30s 全量同步 / 60s 兜底收敛。
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
	// 先回写 DB（锁外：持 reloadMu 做 DB 往返会阻塞重载），收集回写成功的账号。
	okIDs := make([]int64, 0, len(accs))
	for _, ww := range accs {
		if err := s.loader.UpdateAccountStatus(context.Background(), ww.id, ww.status, ww.cooldown, ww.lastErr, ww.weight); err != nil {
			if s.log != nil {
				s.log.Warn("account status writeback failed", logx.Int64("account_id", ww.id), logx.Error(err))
			}
			continue // 回写失败：DB 状态未变，无变更可传播
		}
		okIDs = append(okIDs, ww.id)
	}
	// 组 id 收集短持 reloadMu（评审 M-1）：groupIDs 的读写纪律是"仅经 reloadMu"
	// （buildSnapshots/InvalidateGroup 的 removeGid 就地改写），裸读与之并发是
	// 数据竞态。回写循环非热路径，与 reload 锁竞争不敏感。
	s.reloadMu.Lock()
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	gidSet := make(map[int64]struct{})
	for _, id := range okIDs {
		if as, ok := byID[id]; ok {
			for _, g := range as.groupIDs {
				gidSet[g] = struct{}{}
			}
		}
	}
	s.reloadMu.Unlock()
	if len(gidSet) > 0 && s.cfg.GroupPub != nil {
		gids := make([]int64, 0, len(gidSet))
		for g := range gidSet {
			gids = append(gids, g)
		}
		s.cfg.GroupPub.PublishGroups(context.Background(), gids)
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
	groups, byID := buildSnapshots(m, s.cfg.DefaultMaxConcurrency)
	// 在途并发继承：重建快照会把 concurrency 归零，跨 reload 的在途请求结束后
	// Release 命中新快照 → Add(-1) 把计数拉成负数（管理页并发列显示负值）。
	// Store/Add 均原子、无竞态窗口：继承前旧快照上的 Release 计入旧值后被继承，
	// 继承后新快照上的 Release 正常递减。
	if old, ok := s.store.byID.Load().(map[int64]*accountSnapshot); ok {
		for id, as := range byID {
			if oa, ok := old[id]; ok {
				as.concurrency.Store(oa.concurrency.Load())
			}
		}
	}
	s.store.store(groups, byID)
	return nil
}

// buildSnapshots 构建全量快照：**每账号一个共享实例**——多组账号在多个组
// 快照中引用同一实例（O2 评审实证修复：此前每 (组, 账号) 一个实例，组路由
// Select 与 byID Release 命中不同计数器 → 并发计数分裂漂移 → 槽位假满
// "no available account"，e2e 场景 4 实证；去抖消除"每变更全量重载"后暴露）。
// 组级重载（InvalidateGroup）依赖 groupIDs 跨组引用替换，纪律同此。
func buildSnapshots(m map[int64][]*domain.Account, defaultMax int) (map[int64]*groupSnapshot, map[int64]*accountSnapshot) {
	groups := make(map[int64]*groupSnapshot, len(m))
	byID := make(map[int64]*accountSnapshot)
	for gid, accs := range m {
		gs := &groupSnapshot{}
		for _, a := range accs {
			as, ok := byID[a.ID]
			if !ok {
				as = &accountSnapshot{gid: gid, acc: *a, tpl: a.Template, groupIDs: []int64{gid}}
				as.state.Store(&accState{status: a.Status, cooldownUntil: a.CooldownUntil})
				if a.MaxConcurrency <= 0 {
					as.acc.MaxConcurrency = defaultMax
				}
				byID[a.ID] = as
			} else {
				// 多组账号：复用已建实例并登记本组（共享实例的 gid = 首个组；
				// 数据同源——同一 DB 行的多组引用）。
				as.groupIDs = append(as.groupIDs, gid)
			}
			gs.accounts = append(gs.accounts, as)
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

// InvalidateGroup 组级定向重载（O2 接线矩阵：账号变更 → 受影响组）。与全量
// reload 同一"每账号共享实例"纪律：重载组的新实例同时替换 byID 与其账号的
// 其它组引用——Select（经组路由）与 Release（经 byID）必须命中同一计数器，
// 否则多组账号并发计数分裂漂移 → 槽位假满（O2 实证修复）。账号从组移除且
// 不再属于任何组 → 从 byID 移除；仍属其它组 → 保留实例并摘除本组引用。
// groupIDs 仅经 reloadMu 读写（buildSnapshots/本方法/processWrite 发布收集——
// 评审 M-1 后 processWrite 也持锁读），无锁外读者。
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
	gs, _ := buildSnapshots(map[int64][]*domain.Account{groupID: accs}, s.cfg.DefaultMaxConcurrency)
	newAccs := gs[groupID].accounts
	// 直接复用 buildSnapshots 产出的快照：accounts 与 routes 一并生效，
	// 避免组级重载后 routes 为 nil（Select 预生成路径断裂）。
	newM := make(map[int64]*groupSnapshot, len(m))
	for k, v := range m {
		newM[k] = v
	}
	newM[groupID] = gs[groupID]
	newByID := make(map[int64]*accountSnapshot, len(byID)+len(newAccs))
	for k, v := range byID {
		newByID[k] = v
	}
	// 从组移除的账号（旧组有、新组无）：仍属其它组 → 保留实例并摘本组引用；
	// 已不属于任何组 → 从 byID 删除（其它组引用随实例保留/删除，路由无需重建）。
	// 评审 M-2：先建 新组账号ID 索引再单遍扫描——嵌套循环对 50k 大组批量删
	// 25k 是 ≈1.25e9 次比较 ≈1s 停顿（去抖单 goroutine 内拉大所有失效延迟/
	// 新用户 402 窗口），索引后 O(旧组大小)。
	if old, ok := m[groupID]; ok {
		newIDs := make(map[int64]struct{}, len(newAccs))
		for _, ns := range newAccs {
			newIDs[ns.acc.ID] = struct{}{}
		}
		for _, os := range old.accounts {
			if _, stillIn := newIDs[os.acc.ID]; stillIn {
				continue
			}
			os.groupIDs = removeGid(os.groupIDs, groupID)
			if len(os.groupIDs) == 0 {
				delete(newByID, os.acc.ID)
			}
		}
	}
	// 新实例替换 byID + 其它组引用（多组账号：旧实例在其它组路由中的位置换成
	// 新实例并重建该组路由——共享实例纪律；单组账号 otherGids 为空，零开销）。
	// 评审 M-2：其它组引用替换同禁嵌套扫描——每其它组先建 账号ID→位置 索引
	// （O(该组大小)），替换 O(1)，总量 O(受影响组账号和)。
	type ogRef struct {
		gs  *groupSnapshot
		idx map[int64]int
	}
	otherRefs := make(map[int64]*ogRef)
	for _, ns := range newAccs {
		var otherGids []int64
		if oa, ok := byID[ns.acc.ID]; ok {
			// 在途并发继承（与 reload 同纪律）：重建把计数归零，保留账号的在途
			// 请求 Release 命中新实例 → 继承旧计数避免拉成负数。
			ns.concurrency.Store(oa.concurrency.Load())
			for _, g := range oa.groupIDs {
				if g != groupID {
					otherGids = append(otherGids, g)
				}
			}
		}
		ns.groupIDs = append([]int64{groupID}, otherGids...)
		newByID[ns.acc.ID] = ns
		for _, og := range otherGids {
			if _, ok := otherRefs[og]; ok {
				continue
			}
			ogp, ok := newM[og]
			if !ok {
				continue
			}
			ref := &ogRef{gs: ogp, idx: make(map[int64]int, len(ogp.accounts))}
			for i, oas := range ogp.accounts {
				ref.idx[oas.acc.ID] = i
			}
			otherRefs[og] = ref
		}
	}
	for og, ref := range otherRefs {
		repl := make([]*accountSnapshot, len(ref.gs.accounts))
		copy(repl, ref.gs.accounts)
		for _, ns := range newAccs {
			if i, ok := ref.idx[ns.acc.ID]; ok {
				repl[i] = ns
			}
		}
		newM[og] = &groupSnapshot{accounts: repl, routes: buildRoutes(repl)}
	}
	s.store.store(newM, newByID)
}

// removeGid 摘除 groupIDs 中的指定组（实例共享纪律：组级重载的从组移除路径）。
func removeGid(gids []int64, gid int64) []int64 {
	out := gids[:0]
	for _, g := range gids {
		if g != gid {
			out = append(out, g)
		}
	}
	return out
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

// InvalidateAllSyncCtx 同步全量重载（响应 ctx 取消；#14 T3a 评审 M-2：notify
// Dispatcher.FullRefresh 用——断线重连的全量刷新不得耗尽停机预算）。
func (s *Scheduler) InvalidateAllSyncCtx(ctx context.Context) error { return s.reload(ctx) }

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

// errMsgOr 错误文本回退：errMsg 非空（且截断后非空）用它，否则用默认文案
// （旧语义：429/error 状态机的硬编码 last_error；无文本事件保持原样）。
func errMsgOr(def, errMsg string) string {
	if t := domain.TruncateErrMsg(errMsg); t != "" {
		return t
	}
	return def
}

// FlushRules 同步处理规则引擎队列中的全部事件（仅测试与优雅关闭用）：
// MarkResult 为异步投递，需要立即断言快照的测试先排空队列。
func (s *Scheduler) FlushRules() {
	s.rule.Flush(context.Background())
}

// apply 是规则引擎的动作应用回调（New 时注册）：更新快照状态/冷却/权重、
// EWMA（仅状态类动作）、权重变更时重建组路由（weightedSeq 预生成缓存）、
// 异步 DB 回写。st 为 nil = 只改权重/冷却，不动状态与 EWMA。
// errMsg 为事件错误文本（部署故障修复）：429/unhealthy 落 last_error 用——
// 有文本用文本（域内截断 500），无文本回退既有硬编码文案（旧语义不变）。
func (s *Scheduler) apply(aid int64, st *domain.AccountStatus, cooldownUntil *time.Time, weight *int, errMsg string) {
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
			next.lastError = strPtr(errMsgOr("upstream 429 rate limited", errMsg))
		case domain.StatusUnhealthy:
			next.errCount++
			next.lastError = strPtr(errMsgOr("upstream error", errMsg))
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
		// 评审 I-2：多组账号共享实例只重建首个组（a.gid）的路由——其它组的
		// 路由保留旧权重序列，经 ≤30s 全量同步 / 账号变更组级重载自愈，
		// 非回归（预生成序列的固有折衷：热路径零计算，代价是弱一致性窗口）。
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
