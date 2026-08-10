// Package rule 实现规则引擎（可编排状态管理）：事件 → 有界 channel → 规则 worker
// （priority 首中匹配）→ 状态/冷却/权重更新。scheduler.MarkResult 的硬编码状态机
// 由本包替代：scheduler 只做禁用守卫 + 事件投递（条件投递），动作应用经 SetApply
// 注册的回调完成（更新快照 + EWMA + 异步回写，属 scheduler 侧）。
package rule

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/pkg/logx"
)

// Kind 事件类别（与 scheduler.ResultKind 的映射由 scheduler 侧完成）。
type Kind int

const (
	KindOK Kind = iota
	Kind429
	KindError
)

func (k Kind) String() string {
	switch k {
	case KindOK:
		return "ok"
	case Kind429:
		return "429"
	case KindError:
		return "error"
	}
	return "unknown"
}

// kindFromString when.kind 字符串 → 事件类别；未知值返回负值（永不匹配，
// 非法 kind 在 ValidateWhen 已被拒绝）。
func kindFromString(s string) Kind {
	switch s {
	case "ok":
		return KindOK
	case "429":
		return Kind429
	case "error":
		return KindError
	}
	return -1
}

// Event 请求结果事件（由 scheduler.MarkResult 构造投递）。
type Event struct {
	AccountID    int64
	TemplateID   int64
	GroupID      *int64
	Model        string
	Kind         Kind
	HTTPStatus   *int
	ErrorMessage string
	ResetAt      *time.Time
	OccurredAt   time.Time // 零值由引擎填充为当前时间
}

// ApplyFunc 动作应用回调（由 scheduler 注册）：st 为 nil = 不改状态（只改权重）；
// cooldownUntil 为 nil = 不设冷却；weight 为 nil = 不改权重。errMsg 为事件
// 错误文本（error_message_contains 已匹配；供 last_error 落库——部署故障
// 修复：scheduler 侧截断 500 后回写）。
type ApplyFunc func(aid int64, st *domain.AccountStatus, cooldownUntil *time.Time, weight *int, errMsg string)

// Config 引擎配置。
type Config struct {
	EventQueueSize int // 事件队列容量，默认 4096
}

// defaultWindowSeconds 未配 window_seconds 的规则默认统计窗口。
const defaultWindowSeconds = 60

// RuleEngine 规则引擎：加载 enabled 规则（priority 升序）、逐规则首中匹配、
// 窗口计数维护与 worker 消费循环（Name/Start/Close 见 worker.go）。
type RuleEngine struct {
	cfg   Config
	store repository.RuleStore
	log   *logx.Logger

	ch      chan Event
	dropped atomic.Uint64

	apply   ApplyFunc
	applyMu sync.RWMutex

	rules   []domain.Rule // enabled、priority 升序
	rulesMu sync.RWMutex

	needsOK atomic.Bool

	wm windowMap

	timeNow   func() time.Time
	startOnce atomic.Bool
}

// New 只建结构（不加载规则、不注册 apply——分别由 Reload/SetApply 显式完成）。
func New(cfg Config, store repository.RuleStore, log *logx.Logger) *RuleEngine {
	q := cfg.EventQueueSize
	if q <= 0 {
		q = 4096
	}
	return &RuleEngine{
		cfg:     cfg,
		store:   store,
		log:     log,
		ch:      make(chan Event, q),
		timeNow: time.Now,
	}
}

// SetApply 注册动作应用回调（scheduler 构造期注入；可重复调用覆盖）。
func (e *RuleEngine) SetApply(fn ApplyFunc) {
	e.applyMu.Lock()
	defer e.applyMu.Unlock()
	e.apply = fn
}

// NeedsOKEvents 规则表中是否存在需要 ok 事件投递的规则（when.kind 为 nil 或 "ok"）——
// scheduler 据此条件投递（C1：种子恢复规则 kind=ok 必须投递，否则成功恢复永不触发）。
func (e *RuleEngine) NeedsOKEvents() bool { return e.needsOK.Load() }

// Reload 全量加载 enabled 规则（priority 升序）；空表先写种子（seedRules）。
// 失败返回 error——规则表是状态管理唯一路径，main 收到错误即 fatalf。
func (e *RuleEngine) Reload(ctx context.Context) error {
	rules, err := e.store.ListRules(ctx, boolPtr(true))
	if err != nil {
		return fmt.Errorf("rule engine reload: %w", err)
	}
	if len(rules) == 0 {
		if err := e.seedRules(ctx); err != nil {
			return err
		}
		rules, err = e.store.ListRules(ctx, boolPtr(true))
		if err != nil {
			return fmt.Errorf("rule engine reload after seed: %w", err)
		}
	}
	// store 契约已保证 priority 升序；防御性再排一次。
	slices.SortFunc(rules, func(a, b domain.Rule) int { return a.Priority - b.Priority })

	needsOK := false
	maxWindow := 0
	for _, r := range rules {
		if r.When.Kind == nil || *r.When.Kind == "ok" {
			needsOK = true
		}
		if r.When.WindowSeconds != nil && *r.When.WindowSeconds > maxWindow {
			maxWindow = *r.When.WindowSeconds
		}
	}
	e.rulesMu.Lock()
	e.rules = rules
	e.rulesMu.Unlock()
	e.needsOK.Store(needsOK)
	// 窗口重建：覆盖规则集最大窗口；计数清零（重载语义，规则变更后旧计数不可信）。
	e.wm.reset(time.Duration(maxWindow)*time.Second, needsOK)
	return nil
}

// seedRules 规则表为空时写入等价于旧硬编码状态机的种子规则：
// 429 → status=429 + cooldown 30s（原 cfg.Cooldown429 默认值）；
// error → status=unhealthy + cooldown 5s（原 BackoffBase 默认值，指数退避丢弃——
// 升级惩罚由用户规则用滑动窗口表达）；ok → status=active 无冷却。
//
// 多实例种子幂等（设计文档 §1.5 / R2）：两实例同时空表启动 → 双双进入本方法，
// name/priority 唯一约束（ent schema 已有）保证只有一个实例的插入成功；失败方
// 收到 ErrConflict——忽略继续（各实例插入同一份种子，并集收敛为完整三份，
// Reload 随后重列规则集）。不做 SELECT 后再插的"先查后写"（查与写之间仍有
// 竞态窗口，唯一约束兜底才是治本）。
func (e *RuleEngine) seedRules(ctx context.Context) error {
	n, err := e.store.CountRules(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	seeds := []domain.Rule{
		{
			Name: "seed-429", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtr("429")},
			Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")},
		},
		{
			Name: "seed-error", Enabled: true, Priority: 20,
			When: domain.RuleWhen{Kind: strPtr("error")},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), Cooldown: strPtr("5s")},
		},
		{
			Name: "seed-ok", Enabled: true, Priority: 30,
			When: domain.RuleWhen{Kind: strPtr("ok")},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusActive)},
		},
	}
	for _, s := range seeds {
		if _, err := e.store.CreateRule(ctx, s); err != nil {
			if errors.Is(err, repository.ErrConflict) {
				continue // 并发实例已插同名/同 priority 种子（R2）：跳过，并集收敛
			}
			return fmt.Errorf("create seed rule %s: %w", s.Name, err)
		}
	}
	return nil
}

// ReloadRules 规则表全量重载（invalidate.RulesReloader 适配，#14 T3a 装配
// invalidate.Config.Rules）：与 Reload 同一实现——重载清窗口计数，全实例同步
// 执行语义（设计文档 §1.5，NOTIFY Rules:true 远端变更触发）。
func (e *RuleEngine) ReloadRules(ctx context.Context) error { return e.Reload(ctx) }

// HandleEvent 同步处理单个事件：窗口计数 → 逐规则 Match（首中）→ ApplyFunc。
// worker 消费循环与测试共用。命中不清零窗口计数（C2）——滑动自然衰减，
// 升级阶梯（如 60s 内 ≥5 error → 更重惩罚）不被低阈值规则清零阻断。
// 未命中仅更新计数。
func (e *RuleEngine) HandleEvent(ctx context.Context, ev Event) {
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = e.timeNow()
	}
	e.wm.Add(ev)

	e.rulesMu.RLock()
	rules := e.rules
	e.rulesMu.RUnlock()

	for _, r := range rules {
		wc := windowSnapshot{}
		if ruleNeedsWindow(r.When) {
			wc = e.wm.Snapshot(ev.AccountID, ruleWindowSeconds(r.When), ev.OccurredAt)
		}
		if !Match(r.When, ev, wc) {
			continue
		}
		st, cd, w := Apply(r.Then, ev)
		e.applyMu.RLock()
		fn := e.apply
		e.applyMu.RUnlock()
		if fn != nil {
			fn(ev.AccountID, st, cd, w, ev.ErrorMessage)
		}
		return
	}
}

func boolPtr(b bool) *bool                                   { return &b }
func strPtr(s string) *string                                { return &s }
func statusPtr(s domain.AccountStatus) *domain.AccountStatus { return &s }
