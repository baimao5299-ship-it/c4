package rule

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// —— 内存 fake RuleStore（值语义副本，参照 internal/service/fakestore_test.go 风格） ——

type fakeRuleStore struct {
	mu    sync.Mutex
	rules map[int64]domain.Rule
	next  int64
}

func newFakeRuleStore() *fakeRuleStore {
	return &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}
}

func (f *fakeRuleStore) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Rule, 0, len(f.rules))
	for _, r := range f.rules {
		if enabled != nil && r.Enabled != *enabled {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b domain.Rule) int { return a.Priority - b.Priority })
	return out, nil
}

func (f *fakeRuleStore) CreateRule(ctx context.Context, r domain.Rule) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ID = f.next
	f.next++
	f.rules[r.ID] = r
	return r.ID, nil
}

func (f *fakeRuleStore) UpdateRule(ctx context.Context, r domain.Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[r.ID]; !ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, r.ID)
	}
	f.rules[r.ID] = r
	return nil
}

func (f *fakeRuleStore) DeleteRule(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[id]; !ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	delete(f.rules, id)
	return nil
}

func (f *fakeRuleStore) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if _, ok := f.rules[id]; !ok {
			return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
		}
	}
	for _, id := range ids {
		delete(f.rules, id)
	}
	return nil
}

func (f *fakeRuleStore) CountRules(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.rules)), nil
}

// —— 记录型 ApplyFunc ——

type applied struct {
	aid      int64
	status   *domain.AccountStatus
	cooldown *time.Time
	weight   *int
}

type recorder struct {
	mu      sync.Mutex
	applied []applied
}

func (r *recorder) fn(aid int64, st *domain.AccountStatus, cd *time.Time, w *int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, applied{aid: aid, status: st, cooldown: cd, weight: w})
}

func (r *recorder) get() []applied {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.applied)
}

// —— 测试基座 ——

var testBase = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return testBase.Add(time.Duration(sec) * time.Second) }

func evAt(kind Kind, sec int) Event {
	return Event{AccountID: 1, Kind: kind, OccurredAt: at(sec)}
}

func newTestEngine(t *testing.T, rules ...domain.Rule) (*RuleEngine, *fakeRuleStore) {
	t.Helper()
	st := newFakeRuleStore()
	for _, r := range rules {
		_, err := st.CreateRule(context.Background(), r)
		require.NoError(t, err)
	}
	e := New(Config{}, st, nil)
	require.NoError(t, e.Reload(context.Background()))
	return e, st
}

// —— 校验 ——

func TestValidateWhen(t *testing.T) {
	intPtr := func(v int) *int { return &v }
	f64Ptr := func(v float64) *float64 { return &v }
	strPtr := func(s string) *string { return &s }
	cases := []struct {
		name string
		w    domain.RuleWhen
		ok   bool
	}{
		{"empty when matches everything", domain.RuleWhen{}, true},
		{"kind 429", domain.RuleWhen{Kind: strPtr("429")}, true},
		{"bad kind", domain.RuleWhen{Kind: strPtr("banana")}, false},
		{"window zero", domain.RuleWhen{WindowSeconds: intPtr(0)}, false},
		{"negative count", domain.RuleWhen{Count429GE: intPtr(-1)}, false},
		{"negative count total", domain.RuleWhen{CountTotalGE: intPtr(-2)}, false},
		{"ratio over 1", domain.RuleWhen{Ratio429GE: f64Ptr(1.5), CountTotalGE: intPtr(4)}, false},
		{"ratio without total", domain.RuleWhen{Ratio429GE: f64Ptr(0.5)}, false},
		{"ratio error without total", domain.RuleWhen{RatioErrorGE: f64Ptr(0.5)}, false},
		{"ratio with total", domain.RuleWhen{Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4)}, true},
		{"full valid", domain.RuleWhen{
			Kind: strPtr("error"), CountErrorGE: intPtr(2), WindowSeconds: intPtr(30),
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWhen(tc.w)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestValidateThen(t *testing.T) {
	status := domain.Status429
	cooldown := "30s"
	weight := 40
	cases := []struct {
		name string
		t    domain.RuleThen
		ok   bool
	}{
		{"no action", domain.RuleThen{}, false},
		{"status only", domain.RuleThen{Status: &status}, true},
		{"cooldown only", domain.RuleThen{Cooldown: &cooldown}, true},
		{"bad status", domain.RuleThen{Status: statusPtr(domain.AccountStatus("banana"))}, false},
		{"unparseable cooldown", domain.RuleThen{Cooldown: strPtr("30x")}, false},
		{"zero cooldown", domain.RuleThen{Cooldown: strPtr("0s")}, false},
		{"negative cooldown", domain.RuleThen{Cooldown: strPtr("-5s")}, false},
		{"weight over 100", domain.RuleThen{Weight: intPtr(101)}, false},
		{"negative weight", domain.RuleThen{Weight: intPtr(-1)}, false},
		{"weight only", domain.RuleThen{Weight: &weight}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateThen(tc.t)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func intPtr(v int) *int         { return &v }
func f64Ptr(v float64) *float64 { return &v }
func i64Ptr(v int64) *int64     { return &v }

// —— 窗口 ——

func TestWindowAdvanceAndMerge(t *testing.T) {
	var wm windowMap
	wm.reset(60*time.Second, true)                                // 10s × 6 完整桶 + 1 当前桶
	wm.Add(evAt(KindError, 0))                                    // 桶 [0,10)
	wm.Add(evAt(KindError, 5))                                    // 桶 [0,10)
	wm.Add(evAt(KindError, 15))                                   // 桶 [10,20)，推进环
	wm.Add(Event{AccountID: 2, Kind: Kind429, OccurredAt: at(2)}) // 桶 [0,10)

	// 跨窗合并：20s 窗口含 [0,20) 两桶
	require.Equal(t, windowSnapshot{err: 3}, wm.Snapshot(1, 20, at(15)))
	// 请求秒数 < 桶内偏移 → 只统计当前桶（t=0/5 在 [0,10)，不在 5s 窗口）
	require.Equal(t, windowSnapshot{err: 1}, wm.Snapshot(1, 5, at(15)))
	// 请求秒数超覆盖 → 钳制全环
	require.Equal(t, windowSnapshot{err: 3}, wm.Snapshot(1, 600, at(15)))
	// 账号隔离
	require.Equal(t, windowSnapshot{t429: 1}, wm.Snapshot(2, 60, at(15)))

	// 推进到 25s：三事件仍全在 60s 窗口内
	require.Equal(t, windowSnapshot{err: 3}, wm.Snapshot(1, 60, at(25)))

	// 推进到 70s：t=0/5（65s/70s 前）滑出 60s 窗口，t=15（55s 前）仍在
	require.Equal(t, windowSnapshot{err: 1}, wm.Snapshot(1, 60, at(70)))

	// 推进到 81s：t=15（66s 前）滑出
	require.Equal(t, windowSnapshot{}, wm.Snapshot(1, 60, at(81)))
}

// TestWindowDecay 固定粒度近似边界：窗口 [6,36] 不含 [0,5) 桶的早期事件；
// 桶内偏移 < 窗口秒数时，部分重叠桶全计（近似误差 ≤ 一个粒度）。
func TestWindowDecay(t *testing.T) {
	var wm windowMap
	wm.reset(30*time.Second, true) // 5s × 6 完整桶 + 1 当前桶
	wm.Add(evAt(KindError, 0))
	wm.Add(evAt(KindError, 1))
	wm.Add(evAt(KindError, 2))
	wm.Add(evAt(KindError, 31))

	// 窗口 [1,31]：桶 [0,5) 与窗口部分重叠 → 全计（近似），t=0/1/2 仍未衰减
	require.Equal(t, windowSnapshot{err: 4}, wm.Snapshot(1, 30, at(31)))
	// 窗口 [6,36]：桶 [0,5) 完全滑出 → 只剩 t=31
	require.Equal(t, windowSnapshot{err: 1}, wm.Snapshot(1, 30, at(36)))
}

func TestWindowTrackOK(t *testing.T) {
	var wm windowMap
	wm.reset(60*time.Second, true) // needsOK=true：维护 ok 计数
	wm.Add(evAt(KindOK, 0))
	wm.Add(evAt(Kind429, 1))
	require.Equal(t, windowSnapshot{ok: 1, t429: 1}, wm.Snapshot(1, 60, at(2)))

	wm.reset(60*time.Second, false) // needsOK=false：ok 不计数
	wm.Add(evAt(KindOK, 0))
	wm.Add(evAt(Kind429, 1))
	require.Equal(t, windowSnapshot{t429: 1}, wm.Snapshot(1, 60, at(2)))
}

func TestWindowCleanup(t *testing.T) {
	var wm windowMap
	wm.reset(60*time.Second, true) // retention = 2 × 70s = 140s
	wm.Add(evAt(KindError, 0))     // aid1 @0
	wm.Add(Event{AccountID: 2, Kind: KindError, OccurredAt: at(100)})

	wm.cleanup(at(141)) // cutoff = 1s：aid1 过期、aid2 新鲜（时间序扫描即停）
	require.Len(t, wm.lastSeen, 1)
	require.Equal(t, windowSnapshot{}, wm.Snapshot(1, 60, at(141)))
	require.Equal(t, windowSnapshot{err: 1}, wm.Snapshot(2, 60, at(141)))
}

// —— 评估 ——

func TestMatch(t *testing.T) {
	okEv := func(kind Kind) Event { return Event{AccountID: 1, TemplateID: 2, Model: "m1", Kind: kind} }
	gid := i64Ptr(7)
	http500 := 500
	http429 := 429
	cases := []struct {
		name string
		w    domain.RuleWhen
		ev   Event
		wc   windowSnapshot
		want bool
	}{
		{"empty when matches everything", domain.RuleWhen{}, okEv(KindOK), windowSnapshot{}, true},
		{"kind match", domain.RuleWhen{Kind: strPtr("error")}, okEv(KindError), windowSnapshot{}, true},
		{"kind mismatch", domain.RuleWhen{Kind: strPtr("error")}, okEv(KindOK), windowSnapshot{}, false},
		{"http status nil event", domain.RuleWhen{HTTPStatus: &http500}, okEv(KindError), windowSnapshot{}, false},
		{"http status equal", domain.RuleWhen{HTTPStatus: &http429}, Event{HTTPStatus: &http429}, windowSnapshot{}, true},
		{"message contains", domain.RuleWhen{ErrorMessageContains: strPtr("rate limit")},
			Event{ErrorMessage: "upstream 429 rate limited"}, windowSnapshot{}, true},
		{"message not contains", domain.RuleWhen{ErrorMessageContains: strPtr("timeout")},
			Event{ErrorMessage: "upstream 429 rate limited"}, windowSnapshot{}, false},
		{"account id", domain.RuleWhen{AccountID: i64Ptr(1)}, okEv(KindOK), windowSnapshot{}, true},
		{"account id mismatch", domain.RuleWhen{AccountID: i64Ptr(9)}, okEv(KindOK), windowSnapshot{}, false},
		{"template id", domain.RuleWhen{TemplateID: i64Ptr(2)}, okEv(KindOK), windowSnapshot{}, true},
		{"model", domain.RuleWhen{Model: strPtr("m1")}, okEv(KindOK), windowSnapshot{}, true},
		{"model mismatch", domain.RuleWhen{Model: strPtr("m2")}, okEv(KindOK), windowSnapshot{}, false},
		{"group id ev nil", domain.RuleWhen{GroupID: gid}, okEv(KindOK), windowSnapshot{}, false},
		{"count 429 below", domain.RuleWhen{Count429GE: intPtr(2)}, okEv(Kind429), windowSnapshot{t429: 1}, false},
		{"count 429 ok", domain.RuleWhen{Count429GE: intPtr(2)}, okEv(Kind429), windowSnapshot{t429: 2}, true},
		{"count error ok", domain.RuleWhen{CountErrorGE: intPtr(2)}, okEv(KindError), windowSnapshot{err: 3}, true},
		{"count ok below", domain.RuleWhen{CountOKGE: intPtr(2)}, okEv(KindOK), windowSnapshot{ok: 1}, false},
		{"count total below", domain.RuleWhen{CountTotalGE: intPtr(4)}, okEv(KindOK), windowSnapshot{ok: 2, err: 1}, false},
		{"ratio below threshold", domain.RuleWhen{Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4)},
			okEv(Kind429), windowSnapshot{t429: 1, ok: 3}, false},
		{"ratio at threshold", domain.RuleWhen{Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4)},
			okEv(Kind429), windowSnapshot{t429: 2, ok: 2}, true},
		{"ratio total below floor", domain.RuleWhen{Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4)},
			okEv(Kind429), windowSnapshot{t429: 1, ok: 1}, false},
		{"ratio error ok", domain.RuleWhen{RatioErrorGE: f64Ptr(0.75), CountTotalGE: intPtr(4)},
			okEv(KindError), windowSnapshot{err: 3, ok: 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Match(tc.w, tc.ev, tc.wc))
		})
	}
}

func TestApply(t *testing.T) {
	status := domain.StatusActive
	weight := 40
	resetAt := at(90)
	ev := Event{AccountID: 1, Kind: Kind429, OccurredAt: at(10), ResetAt: &resetAt}

	// cooldown = OccurredAt + 30s
	st, cd, w := Apply(domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")}, ev)
	require.NotNil(t, st)
	require.Equal(t, domain.Status429, *st)
	require.Equal(t, at(40), *cd)
	require.Nil(t, w)

	// cooldown 未配 + ResetAt 非 nil → ResetAt（M2 残留语义）
	st, cd, _ = Apply(domain.RuleThen{Status: &status}, ev)
	require.Equal(t, domain.StatusActive, *st)
	require.Equal(t, at(90), *cd)

	// cooldown 优先于 ResetAt
	_, cd, _ = Apply(domain.RuleThen{Cooldown: strPtr("5s")}, ev)
	require.Equal(t, at(15), *cd)

	// 只改权重：status nil；无 cooldown 时 ResetAt 兜底（M2 残留语义）
	st, cd, w = Apply(domain.RuleThen{Weight: &weight}, ev)
	require.Nil(t, st)
	require.Equal(t, at(90), *cd)
	require.Equal(t, 40, *w)

	// 无 cooldown 且无 ResetAt → 无冷却
	_, cd, _ = Apply(domain.RuleThen{Weight: &weight}, Event{AccountID: 1, OccurredAt: at(10)})
	require.Nil(t, cd)

	// 非法 cooldown（校验已挡，防御性跳过）
	_, cd, _ = Apply(domain.RuleThen{Cooldown: strPtr("bogus")}, ev)
	require.Nil(t, cd)
}

// —— 引擎 ——

func TestReloadNeedsOKEvents(t *testing.T) {
	cases := []struct {
		name  string
		when  domain.RuleWhen
		needs bool
	}{
		{"kind nil", domain.RuleWhen{}, true},
		{"kind ok", domain.RuleWhen{Kind: strPtr("ok")}, true},
		{"kind error", domain.RuleWhen{Kind: strPtr("error")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newTestEngine(t, domain.Rule{
				Name: "r", Enabled: true, Priority: 10,
				When: tc.when, Then: domain.RuleThen{Status: statusPtr(domain.StatusActive)},
			})
			require.Equal(t, tc.needs, e.NeedsOKEvents())
		})
	}
}

func TestSeedRules(t *testing.T) {
	st := newFakeRuleStore()
	e := New(Config{}, st, nil)
	require.NoError(t, e.Reload(context.Background()))

	// 种子 3 条：429/30s、error/unhealthy/5s、ok/active，priority 10/20/30
	require.Equal(t, int64(3), mustCount(t, st))
	require.True(t, e.NeedsOKEvents()) // 种子含 kind=ok 恢复规则（C1）

	rules, err := st.ListRules(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, rules, 3)
	require.Equal(t, []int{10, 20, 30}, []int{rules[0].Priority, rules[1].Priority, rules[2].Priority})
	require.Equal(t, "429", *rules[0].When.Kind)
	require.Equal(t, domain.Status429, *rules[0].Then.Status)
	require.Equal(t, "30s", *rules[0].Then.Cooldown)
	require.Equal(t, "error", *rules[1].When.Kind)
	require.Equal(t, domain.StatusUnhealthy, *rules[1].Then.Status)
	require.Equal(t, "5s", *rules[1].Then.Cooldown)
	require.Equal(t, "ok", *rules[2].When.Kind)
	require.Equal(t, domain.StatusActive, *rules[2].Then.Status)
	require.Nil(t, rules[2].Then.Cooldown)

	// 非空表不重复写种子
	require.NoError(t, e.Reload(context.Background()))
	require.Equal(t, int64(3), mustCount(t, st))
}

func mustCount(t *testing.T, st *fakeRuleStore) int64 {
	t.Helper()
	n, err := st.CountRules(context.Background())
	require.NoError(t, err)
	return n
}

func TestPriorityHitOrder(t *testing.T) {
	e, _ := newTestEngine(t,
		domain.Rule{Name: "low", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtr("error")},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), Cooldown: strPtr("5s")}},
		domain.Rule{Name: "high", Enabled: true, Priority: 20,
			When: domain.RuleWhen{Kind: strPtr("error")},
			Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")}},
	)
	var rec recorder
	e.SetApply(rec.fn)

	// 两规则都命中：priority 低者首中，只执行一次
	e.HandleEvent(context.Background(), evAt(KindError, 0))
	app := rec.get()
	require.Len(t, app, 1)
	require.Equal(t, domain.StatusUnhealthy, *app[0].status)
	require.Equal(t, at(5), *app[0].cooldown)

	// ok 事件两规则都不命中
	e.HandleEvent(context.Background(), evAt(KindOK, 1))
	require.Len(t, rec.get(), 1)
}

func TestDisabledRuleNotLoaded(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "off", Enabled: false, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("error")},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429)},
	})
	var rec recorder
	e.SetApply(rec.fn)
	require.False(t, e.NeedsOKEvents())
	e.HandleEvent(context.Background(), evAt(KindError, 0))
	require.Empty(t, rec.get())
}

// TestHitKeepsCountsThenDecays 命中不清零窗口计数（C2）：阈值 2 连续命中两次；
// 滑动衰减后（[0,5) 桶整体滑出 30s 窗口）阈值重新可达。
func TestHitKeepsCountsThenDecays(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "escalate", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("error"), CountErrorGE: intPtr(2), WindowSeconds: intPtr(30)},
		Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
	})
	var rec recorder
	e.SetApply(rec.fn)

	e.HandleEvent(context.Background(), evAt(KindError, 0)) // err=1，未命中
	require.Empty(t, rec.get())
	e.HandleEvent(context.Background(), evAt(KindError, 1)) // err=2，命中
	require.Len(t, rec.get(), 1)
	e.HandleEvent(context.Background(), evAt(KindError, 2)) // err=3（不清零），再命中
	require.Len(t, rec.get(), 2)

	e.HandleEvent(context.Background(), evAt(KindError, 36)) // 窗口 [6,36]：仅 +36，err=1 未命中
	require.Len(t, rec.get(), 2)
}

func TestRatioMatchWithTotalFloor(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "hot-account", Enabled: true, Priority: 10,
		When: domain.RuleWhen{
			Kind: nil, Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4), WindowSeconds: intPtr(30),
		},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")},
	})
	var rec recorder
	e.SetApply(rec.fn)

	// 429×2 + ok×2 → total=4、ratio=0.5 → 命中（needsOK：kind=nil → ok 计数维护）
	e.HandleEvent(context.Background(), evAt(Kind429, 0))
	e.HandleEvent(context.Background(), evAt(Kind429, 1))
	e.HandleEvent(context.Background(), evAt(KindOK, 2))
	e.HandleEvent(context.Background(), evAt(KindOK, 3))
	app := rec.get()
	require.Len(t, app, 1)
	require.Equal(t, domain.Status429, *app[0].status)
	require.Equal(t, at(33), *app[0].cooldown)

	// 样本不足：total=2 < 4 → 比例不参与
	e2, _ := newTestEngine(t, domain.Rule{
		Name: "hot2", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4), WindowSeconds: intPtr(30)},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429)},
	})
	var rec2 recorder
	e2.SetApply(rec2.fn)
	e2.HandleEvent(context.Background(), evAt(Kind429, 0))
	e2.HandleEvent(context.Background(), evAt(Kind429, 1))
	require.Empty(t, rec2.get())
}

// —— worker ——

func TestName(t *testing.T) {
	e := New(Config{}, newFakeRuleStore(), nil)
	require.Equal(t, "rule-engine", e.Name())
}

func TestEnqueueFullDrops(t *testing.T) {
	e := New(Config{EventQueueSize: 1}, newFakeRuleStore(), nil)
	e.Enqueue(evAt(KindError, 0)) // 占满
	require.Equal(t, uint64(0), e.dropped.Load())
	e.Enqueue(evAt(KindError, 1)) // 满 → 丢弃
	e.Enqueue(evAt(KindError, 2))
	require.Equal(t, uint64(2), e.dropped.Load())
	require.Len(t, e.ch, 1)
}

func TestStartTwice(t *testing.T) {
	e := New(Config{}, newFakeRuleStore(), nil)
	require.NoError(t, e.Start(context.Background()))
	require.Error(t, e.Start(context.Background()))
}

func TestCloseDrainsQueue(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "fail", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("error")},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")},
	})
	var rec recorder
	e.SetApply(rec.fn)
	// 未 Start：Close 直接排空
	e.Enqueue(evAt(KindError, 5))
	require.NoError(t, e.Close(context.Background()))
	app := rec.get()
	require.Len(t, app, 1)
	require.Equal(t, domain.Status429, *app[0].status)
	require.Equal(t, at(35), *app[0].cooldown)

	// Close 幂等
	require.NoError(t, e.Close(context.Background()))
}

func TestStartConsumesThenCloseDrains(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "fail", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("error")},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429)},
	})
	var rec recorder
	e.SetApply(rec.fn)

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, e.Start(ctx))
	e.Enqueue(evAt(KindError, 0))
	require.Eventually(t, func() bool { return len(rec.get()) == 1 }, 5*time.Second, 10*time.Millisecond)

	// 取消后：loop 退出，Close 排空剩余
	cancel()
	e.Enqueue(evAt(KindError, 1))
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	require.NoError(t, e.Close(closeCtx))
	require.Eventually(t, func() bool { return len(rec.get()) == 2 }, 5*time.Second, 10*time.Millisecond)
}
