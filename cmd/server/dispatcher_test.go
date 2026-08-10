package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/invalidate"
	"go-proxy-mini/internal/notify"
)

// --- 测试重载目标（参照 internal/invalidate/invalidate_test.go 的 rec* 系列） ---

type recSched2 struct {
	mu     sync.Mutex
	full   int
	groups []int64
}

func (r *recSched2) InvalidateAll() { r.mu.Lock(); r.full++; r.mu.Unlock() }
func (r *recSched2) InvalidateGroup(g int64) {
	r.mu.Lock()
	r.groups = append(r.groups, g)
	r.mu.Unlock()
}
func (r *recSched2) InvalidateAllSync() error { r.mu.Lock(); r.full++; r.mu.Unlock(); return nil }
func (r *recSched2) counts() (int, []int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.full, append([]int64(nil), r.groups...)
}

type recClients2 struct {
	mu sync.Mutex
	n  int
}

func (r *recClients2) InvalidateAll() { r.mu.Lock(); r.n++; r.mu.Unlock() }
func (r *recClients2) calls() int     { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

type recAuth2 struct {
	mu sync.Mutex
	n  int
}

func (r *recAuth2) Reload(ctx context.Context) error { r.mu.Lock(); r.n++; r.mu.Unlock(); return nil }
func (r *recAuth2) calls() int                       { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

type recBal2 struct {
	mu   sync.Mutex
	rel  int
	mult int
}

func (r *recBal2) Reload(ctx context.Context) error { r.mu.Lock(); r.rel++; r.mu.Unlock(); return nil }
func (r *recBal2) ReloadMultipliers(ctx context.Context) error {
	r.mu.Lock()
	r.mult++
	r.mu.Unlock()
	return nil
}
func (r *recBal2) relCalls() int  { r.mu.Lock(); defer r.mu.Unlock(); return r.rel }
func (r *recBal2) multCalls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.mult }

type recSettings2 struct {
	mu sync.Mutex
	n  int
}

func (r *recSettings2) ReloadSettings(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return nil
}
func (r *recSettings2) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

type recRules2 struct {
	mu sync.Mutex
	n  int
}

func (r *recRules2) ReloadRules(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return nil
}
func (r *recRules2) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// newTestDispatcher 构造真实 Debouncer（1ms 窗口，测试快刷）+ 记录 fake 的
// dispatcher 与各目标句柄。
type testDispRig struct {
	d        *dispatcher
	inv      *invalidate.Debouncer
	sched    *recSched2
	clients  *recClients2
	auth     *recAuth2
	bal      *recBal2
	settings *recSettings2
	rules    *recRules2
	cancel   context.CancelFunc
}

func newTestDispatcher(t *testing.T) *testDispRig {
	t.Helper()
	sched := &recSched2{}
	clients := &recClients2{}
	auth := &recAuth2{}
	bal := &recBal2{}
	settings := &recSettings2{}
	rules := &recRules2{}
	inv := invalidate.New(invalidate.Config{
		Window:   time.Millisecond,
		Sched:    sched,
		Clients:  clients,
		Auth:     auth,
		Balances: bal,
		Rules:    rules,
	})
	inv.SetSettings(settings)
	d := &dispatcher{inv: inv, auth: auth, balances: bal, sched: sched, svc: settings, rules: rules}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	return &testDispRig{d: d, inv: inv, sched: sched, clients: clients, auth: auth, bal: bal, settings: settings, rules: rules, cancel: cancel}
}

// waitFlush 等去抖窗口 flush 完成：轮询直到谓词满足或超时。
func waitFlush(t *testing.T, pred func() bool) {
	t.Helper()
	require.Eventually(t, pred, 2*time.Second, 5*time.Millisecond)
}

// TestDispatcherApplyMapping Dispatcher.Apply 映射表（设计文档 §2.2）：
// 每个 Change 位 → 对应去抖器 Mark → 一次 flush 内命中对应重载目标。
func TestDispatcherApplyMapping(t *testing.T) {
	t.Run("Users", func(t *testing.T) {
		rg := newTestDispatcher(t)
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Users: true}))
		waitFlush(t, func() bool { return rg.auth.calls() > 0 })
		require.Equal(t, 1, rg.auth.calls(), "users → auth Reload 一次")
	})

	t.Run("Templates", func(t *testing.T) {
		rg := newTestDispatcher(t)
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Templates: true}))
		waitFlush(t, func() bool { f, g := rg.sched.counts(); return f > 0 || len(g) > 0 })
		full, groups := rg.sched.counts()
		require.Equal(t, 1, full, "templates → sched 全量")
		require.Empty(t, groups)
		require.Equal(t, 1, rg.clients.calls(), "templates → clients 失效")
	})

	t.Run("Groups", func(t *testing.T) {
		rg := newTestDispatcher(t)
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Groups: []int64{10, 20}}))
		waitFlush(t, func() bool { _, g := rg.sched.counts(); return len(g) > 0 })
		_, groups := rg.sched.counts()
		require.ElementsMatch(t, []int64{10, 20}, groups, "groups → sched 组级定向")
		require.Equal(t, 0, rg.clients.calls(), "纯组级变更不碰 clients")
	})

	t.Run("GroupsWithClients", func(t *testing.T) {
		rg := newTestDispatcher(t)
		// 账号 upstream_key 变更：组级 + clients 同批（service account.go 发布点形态）
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Groups: []int64{10}, Clients: true}))
		waitFlush(t, func() bool { _, g := rg.sched.counts(); return len(g) > 0 && rg.clients.calls() > 0 })
		_, groups := rg.sched.counts()
		require.ElementsMatch(t, []int64{10}, groups)
		require.Equal(t, 1, rg.clients.calls())
	})

	t.Run("StandaloneClients", func(t *testing.T) {
		rg := newTestDispatcher(t)
		// 防御性兜底：服务端发布点恒与 Templates/Groups 并排，独立 Clients 也映射
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Clients: true}))
		waitFlush(t, func() bool { return rg.clients.calls() > 0 })
		full, groups := rg.sched.counts()
		require.Equal(t, 0, full, "独立 clients 不触发 sched 重载")
		require.Empty(t, groups)
		require.Equal(t, 1, rg.clients.calls())
	})

	t.Run("Multipliers", func(t *testing.T) {
		rg := newTestDispatcher(t)
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Multipliers: true}))
		waitFlush(t, func() bool { return rg.bal.multCalls() > 0 })
		require.Equal(t, 1, rg.bal.multCalls(), "multipliers → 余额倍率定向刷新")
	})

	t.Run("Keys", func(t *testing.T) {
		rg := newTestDispatcher(t)
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Keys: true}))
		waitFlush(t, func() bool { return rg.auth.calls() > 0 })
		require.Equal(t, 1, rg.auth.calls(), "keys → auth 快照全量 Reload")
	})

	t.Run("Settings", func(t *testing.T) {
		rg := newTestDispatcher(t)
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Settings: true}))
		waitFlush(t, func() bool { return rg.settings.calls() > 0 })
		require.Equal(t, 1, rg.settings.calls(), "settings → settings 快照重载")
	})

	t.Run("Rules", func(t *testing.T) {
		rg := newTestDispatcher(t)
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Rules: true}))
		waitFlush(t, func() bool { return rg.rules.calls() > 0 })
		require.Equal(t, 1, rg.rules.calls(), "rules → 规则表全量重载")
	})

	t.Run("DegradedFullWithGroups", func(t *testing.T) {
		rg := newTestDispatcher(t)
		// 载荷守卫降级形态（Groups 超限 → Templates=true，R9）：组级被全量包含
		// 跳过，语义仍正确（不重复组级重载）。
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Templates: true, Groups: []int64{10}}))
		waitFlush(t, func() bool { f, _ := rg.sched.counts(); return f > 0 })
		full, groups := rg.sched.counts()
		require.Equal(t, 1, full)
		require.Empty(t, groups, "Templates 存在时组级被包含跳过（去抖器 merge 语义）")
	})

	t.Run("EmptyChange", func(t *testing.T) {
		rg := newTestDispatcher(t)
		// 空 Change：无任何标记（service.publish 已判空跳过，双保险）
		require.NoError(t, rg.d.Apply(context.Background(), notify.Change{}))
		time.Sleep(5 * time.Millisecond)
		require.Equal(t, 0, rg.auth.calls())
		full, groups := rg.sched.counts()
		require.Equal(t, 0, full)
		require.Empty(t, groups)
		require.Equal(t, 0, rg.clients.calls())
		require.Equal(t, 0, rg.bal.multCalls())
		require.Equal(t, 0, rg.settings.calls())
		require.Equal(t, 0, rg.rules.calls())
	})
}

// TestDispatcherApplyMergesSingleFlush 本地+远端同窗口合并：一条 NOTIFY 与
// 本地变更落入同一去抖窗口 → 一次 flush，不重复 reload（设计文档 §2.3）。
func TestDispatcherApplyMergesSingleFlush(t *testing.T) {
	rg := newTestDispatcher(t)
	require.NoError(t, rg.d.Apply(context.Background(), notify.Change{Users: true}))
	rg.inv.Users() // 模拟本地 admin 变更（同一去抖器）
	waitFlush(t, func() bool { return rg.auth.calls() > 0 })
	require.Equal(t, 1, rg.auth.calls(), "远端 + 本地同窗口合并为一次 reload")
}

// TestDispatcherFullRefresh FullRefresh 覆盖全部五路重载（设计文档 §2.3）：
// auth + 余额 + sched 全量 + settings + 规则；billing 关闭（balances nil）→
// 跳过余额路径。
func TestDispatcherFullRefresh(t *testing.T) {
	t.Run("all", func(t *testing.T) {
		rg := newTestDispatcher(t)
		require.NoError(t, rg.d.FullRefresh(context.Background()))
		require.Equal(t, 1, rg.auth.calls())
		require.Equal(t, 1, rg.bal.relCalls(), "balances Reload 一次")
		full, _ := rg.sched.counts()
		require.Equal(t, 1, full, "sched InvalidateAllSync 一次")
		require.Equal(t, 1, rg.settings.calls())
		require.Equal(t, 1, rg.rules.calls())
	})

	t.Run("billingDisabled", func(t *testing.T) {
		auth := &recAuth2{}
		sched := &recSched2{}
		settings := &recSettings2{}
		rules := &recRules2{}
		inv := invalidate.New(invalidate.Config{Window: time.Millisecond, Sched: sched, Clients: &recClients2{}, Auth: auth, Rules: rules})
		inv.SetSettings(settings)
		d := &dispatcher{inv: inv, auth: auth, balances: nil, sched: sched, svc: settings, rules: rules}
		require.NoError(t, d.FullRefresh(context.Background()))
		require.Equal(t, 1, auth.calls())
		require.Equal(t, 1, settings.calls())
		require.Equal(t, 1, rules.calls())
		full, _ := sched.counts()
		require.Equal(t, 1, full)
	})
}
