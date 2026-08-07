package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/rule"
)

// --- 测试 Loader（内存实现） ---

type memLoader struct {
	mu      sync.Mutex
	byGroup map[int64][]*domain.Account
	writes  []statusWrite
}

func newMemLoader(byGroup map[int64][]*domain.Account) *memLoader {
	return &memLoader{byGroup: byGroup}
}

func (m *memLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64][]*domain.Account, len(m.byGroup))
	for k, v := range m.byGroup {
		out[k] = v
	}
	return out, nil
}

func (m *memLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byGroup[id], nil
}

func (m *memLoader) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldown *time.Time, lastErr *string, weight *int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes = append(m.writes, statusWrite{id: id, status: status, cooldown: cooldown, weight: weight})
	return nil
}

func testCfg() Config {
	return Config{
		DefaultMaxConcurrency: 2,
		SyncInterval:          100 * time.Hour, // 测试中不触发定时同步
	}
}

// fakeRuleStore 内存 RuleStore：种子写入 + 列表查询（值语义副本）。
type fakeRuleStore struct {
	mu    sync.Mutex
	rules map[int64]domain.Rule
	next  int64
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
	f.rules[r.ID] = r
	return nil
}

func (f *fakeRuleStore) DeleteRule(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rules, id)
	return nil
}

func (f *fakeRuleStore) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

var _ repository.RuleStore = (*fakeRuleStore)(nil)

func intPtr(v int) *int { return &v }

func tpl(id int64, format domain.RequestFormat, models []string) *domain.Template {
	return &domain.Template{ID: id, BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{format}, Models: models}
}

func acc(id int64, t *domain.Template, maxConc int) *domain.Account {
	return &domain.Account{ID: id, TemplateID: t.ID, Template: t, UpstreamKey: "k", Status: domain.StatusActive, Weight: 100, MaxConcurrency: maxConc}
}

func newSched(t *testing.T, m *memLoader) *Scheduler {
	t.Helper()
	// 规则引擎：空表 Reload 写种子（429/30s、error/unhealthy/5s、ok/active），
	// 行为等价于旧硬编码状态机（Task C 改造后 MarkResult 走规则路径）。
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))
	return s
}

// newTestScheduler 用给定账号（固定放入组 10）构建已加载快照的调度器。
func newTestScheduler(t *testing.T, accs []*domain.Account) *Scheduler {
	t.Helper()
	return newSched(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
}

func TestSelectFormatHardFilter(t *testing.T) {
	chat := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	ant := tpl(2, domain.FormatAnthropic, []string{"claude"})
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, chat, 4), acc(2, ant, 4)},
	})
	s := newSched(t, m)

	// anthropic 路径下只命中 anthropic 模板账号
	sel, err := s.Select(10, domain.FormatAnthropic, "claude")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID)
	s.Release(sel.AccountID)

	// 格式不匹配（组内只有 chat 模板）→ ErrFormatUnavailable
	m2 := newMemLoader(map[int64][]*domain.Account{10: {acc(1, chat, 4)}})
	s2 := newSched(t, m2)
	_, err = s2.Select(10, domain.FormatOpenAIResponses, "gpt-4o")
	require.ErrorIs(t, err, ErrFormatUnavailable)
}

func TestSelectModelPreference(t *testing.T) {
	// 两账号同格式：一个 Serves(model)，一个不
	tA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	tB := tpl(2, domain.FormatOpenAIChat, []string{"other"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tA, 4), acc(2, tB, 4)}})
	s := newSched(t, m)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID, "model preference tier")
}

func TestConcurrencyLimit(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 1)}})
	s := newSched(t, m)
	sel1, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable)
	s.Release(sel1.AccountID)
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "available after release")
}

func TestMark429CooldownAndRecover(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	// 启动异步回写循环（否则 statusWrite 永远不会被消费）
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.writebackLoop(ctx)

	// 种子规则：429 → status=429 + cooldown 30s（MarkResult 异步投递，flush 同步处理）
	s.MarkResult(1, Result429, nil, 0, "")
	s.FlushRules()
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "in cooldown should be unavailable")
	// 冷却过期后惰性恢复（种子 cooldown 30s > 15s，需推进 35s）
	s.timeNow = func() time.Time { return time.Now().Add(35 * time.Second) }
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "available after cooldown")
	s.MarkResult(sel.AccountID, ResultOK, nil, 0, "")
	s.FlushRules()
	s.Release(sel.AccountID)
	// C-M2 语义钉：OK 恢复 active 但残留 cooldownUntil 保留至过期（新 apply 仅
	// cooldownUntil 非 nil 才设置；种子 ok 规则无 cooldown → 旧冷却不清除）。
	ri, _ := s.Runtime(1)
	require.Equal(t, domain.StatusActive, ri.Status, "OK 恢复 active")
	require.NotNil(t, ri.CooldownUntil, "OK 不清除残留冷却（保留至过期，Select 按时间判定不受影响）")
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.writes) > 0
	}, time.Second, 10*time.Millisecond, "expected async status write")
}

func TestMarkErrorBackoff(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	// 种子规则：error → unhealthy + cooldown 5s（指数退避已废弃——升级惩罚由规则表达）
	s.MarkResult(1, ResultError, nil, 0, "")
	s.FlushRules()
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status)
	require.Equal(t, 1, ri.ErrCount)
	require.NotNil(t, ri.CooldownUntil)
	require.True(t, ri.CooldownUntil.After(time.Now().Add(4*time.Second)), "seed cooldown 5s applied")
	s.MarkResult(1, ResultError, nil, 0, "")
	s.FlushRules()
	ri, _ = s.Runtime(1)
	require.Equal(t, 2, ri.ErrCount)
	s.MarkResult(1, ResultOK, nil, 0, "")
	s.FlushRules()
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusActive, ri.Status, "success resets status")
	require.Equal(t, 0, ri.ErrCount, "success resets err count")
}

func TestSelectUnknownGroup(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{})
	s := newSched(t, m)
	_, err := s.Select(99, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrGroupNotFound)
}

func TestInvalidateGroupReloads(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], acc(2, tplx, 4))
	m.mu.Unlock()
	s.InvalidateGroup(10) // 同步 reload
	// 账号 2 已进入候选。两账号并发上限各 4，不释放地连续选 5 次必须全部成功
	//（账号 1 单独最多承接 4 次 → 第 5 次必由账号 2 承接；若 reload 未生效则第 5 次 ErrNoAvailable），
	// 且由鸽巢原理两个账号都至少被选中一次（各 ≤4，总数 5）。
	var sels []*Selection
	for i := 0; i < 5; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
		require.NoError(t, err, "5 selects must succeed with both accounts in pool")
		sels = append(sels, sel)
	}
	var has1, has2 bool
	for _, sel := range sels {
		has1 = has1 || sel.AccountID == 1
		has2 = has2 || sel.AccountID == 2
	}
	require.True(t, has1 && has2, "both accounts should serve")
	for _, sel := range sels {
		s.Release(sel.AccountID)
	}
}

// TestInvalidateGroupByIDRebuild 回归：InvalidateGroup 后 byID 必须与 groups 同步重建。
// 组内账号 [1,2] → [2,3]（1 移除、3 新增）：Release/MarkResult 必须命中新快照，
// 被移除账号必须从 byID 消失（no-op 安全）。
func TestInvalidateGroupByIDRebuild(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 2), acc(2, tplx, 2)}})
	s := newSched(t, m)

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{acc(2, tplx, 2), acc(3, tplx, 2)}
	m.mu.Unlock()
	s.InvalidateGroup(10)

	// 占满并发：两账号各上限 2、总容量 4 → 4 次选择后各持 2 个槽（鸽巢原理，确定性）。
	var sels []*Selection
	for i := 0; i < 4; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
		require.NoError(t, err)
		sels = append(sels, sel)
	}
	// 释放 2/3 的槽：并发计数必须在新快照上递减（byID 指向旧快照则计数错位）。
	for _, sel := range sels {
		s.Release(sel.AccountID)
	}
	ri, ok := s.Runtime(2)
	require.True(t, ok)
	require.Equal(t, int64(0), ri.Concurrency, "retained account release hits the new snapshot")
	ri, ok = s.Runtime(3)
	require.True(t, ok, "added account must be in byID")
	require.Equal(t, int64(0), ri.Concurrency, "added account release hits the new snapshot")

	// 新增账号 3 的结果回流必须落新快照并触发回写。
	s.MarkResult(3, ResultError, nil, 0, "")
	s.FlushRules()
	ri, _ = s.Runtime(3)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "markresult hits the new snapshot")
	require.Equal(t, 1, ri.ErrCount)

	// 被移除账号 1：Runtime 不可见，MarkResult/Release 安全 no-op（无回写）。
	_, ok = s.Runtime(1)
	require.False(t, ok, "removed account must not be in byID")
	s.MarkResult(1, ResultError, nil, 0, "")
	s.FlushRules()
	s.Release(1)

	require.NoError(t, s.Close(context.Background()))
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []int64
	for _, w := range m.writes {
		ids = append(ids, w.id)
	}
	require.Contains(t, ids, int64(3), "writeback fires for the added account")
	require.NotContains(t, ids, int64(1), "no writeback for the removed account")
}

// TestInvalidateGroupShrinkByID 回归：组内账号从 [4,5] 收缩为 [4] 时，
// 保留账号 4 的 byID 必须指向新快照（并发上限 2→1 生效），被移除账号 5 必须消失。
func TestInvalidateGroupShrinkByID(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{11: {acc(4, tplx, 2), acc(5, tplx, 2)}})
	s := newSched(t, m)

	m.mu.Lock()
	m.byGroup[11] = []*domain.Account{acc(4, tplx, 1)} // 5 移除；4 的并发上限 2→1
	m.mu.Unlock()
	s.InvalidateGroup(11)

	sel, err := s.Select(11, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(4), sel.AccountID)
	ri, ok := s.Runtime(4)
	require.True(t, ok)
	require.Equal(t, int64(1), ri.Concurrency, "select hits the new snapshot (max 1)")
	s.Release(sel.AccountID)
	ri, _ = s.Runtime(4)
	require.Equal(t, int64(0), ri.Concurrency, "release hits the new snapshot")

	_, ok = s.Runtime(5)
	require.False(t, ok, "removed account must not be in byID")
	s.MarkResult(5, ResultError, nil, 0, "")
	s.FlushRules()
	s.Release(5)

	require.NoError(t, s.Close(context.Background()))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.writes {
		require.NotEqual(t, int64(5), w.id, "no writeback for the removed account")
	}
}

// TestMarkResultDisabledStaysDisabled 回归：管理端禁用账号后（InvalidateGroup
// 以 disabled 状态重载快照），在途请求的 MarkResult(OK) 不得把账号复活为
// active，也不得回写 DB——否则禁用被静默抹除、30s 同步后账号复现（评审发现）。
func TestMarkResultDisabledStaysDisabled(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	// 在途请求：先选中账号
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)

	// 管理端禁用：以 disabled 状态重载组快照（与账号管理变更同路径）
	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{{
		ID: 1, TemplateID: 1, Template: tplx, UpstreamKey: "k",
		Status: domain.StatusDisabled, Weight: 100, MaxConcurrency: 4,
	}}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "禁用已在快照生效")

	// 在途请求完成：OK 不得把状态重置回 active、不得重置错误计数（守卫同步短路，不投递）
	s.MarkResult(1, ResultOK, nil, 0, "")
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusDisabled, ri.Status, "OK 不得复活禁用账号")
	require.Zero(t, ri.ErrCount, "OK 不得重置禁用账号的错误计数")

	// 防御性：429/错误分支同样不得给禁用账号设置冷却或改写状态
	s.MarkResult(1, Result429, nil, 0, "")
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusDisabled, ri.Status, "429 不得把禁用账号改写为 429")
	require.Nil(t, ri.CooldownUntil, "429 不得给禁用账号设置冷却")
	s.MarkResult(1, ResultError, nil, 0, "")
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusDisabled, ri.Status, "错误分支不得改写禁用账号")

	// 禁用账号不可再被选中
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable)
	s.Release(sel.AccountID)

	// 无回写：Close 排空后 DB 写入列表必须为空
	require.NoError(t, s.Close(context.Background()))
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Empty(t, m.writes, "禁用账号不得有状态回写")
}

// TestWeightActionRebuildsRoutes 权重动作（I1/I5）：命中后快照权重更新 + 组路由
// 重建（weightedSeq 预生成缓存），选号分布立即按新权重；纯 weight 动作不更新
// 状态与 EWMA；DB 回写携带 weight。
func TestWeightActionRebuildsRoutes(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	m := newMemLoader(map[int64][]*domain.Account{10: {
		{ID: 1, TemplateID: 1, Template: tplx, UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 1, Template: tplx, UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	}})
	// 自定义规则表（非种子）：error → 纯 weight 动作（weight 10）
	rstore := &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}
	_, err := rstore.CreateRule(context.Background(), domain.Rule{
		Name: "throttle", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("error")},
		Then: domain.RuleThen{Weight: intPtr(10)},
	})
	require.NoError(t, err)
	re := rule.New(rule.Config{}, rstore, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.writebackLoop(ctx)

	s.MarkResult(1, ResultError, nil, 0, "")
	s.FlushRules()

	// 纯 weight 动作：状态/EWMA 不动，快照权重更新
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status, "纯 weight 动作不动状态")
	require.Zero(t, ri.ErrRate, "纯 weight 动作不更新 EWMA")
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	require.Equal(t, 10, byID[1].acc.Weight, "快照权重已更新")

	// 回写携带 weight
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.writes) == 1 && m.writes[0].weight != nil && *m.writes[0].weight == 10
	}, time.Second, 10*time.Millisecond, "writeback carries weight")

	// 路由序列已重建：选号分布按新权重（100:10 → ≈10:1）
	const n = 20_000
	counts := map[int64]int{}
	for i := 0; i < n; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		counts[sel.AccountID]++
		s.Release(sel.AccountID)
	}
	ratio := float64(counts[2]) / float64(counts[1])
	require.InDelta(t, ratio, 10.0, 0.5, "weight 100:10 → 频率比 ≈ 10:1（路由已按新权重重建）")
}

// TestProcessWriteMergeKeepsWeight 回归（评审 I-1）：同账号 weight 写先入队、
// status 写（weight=nil）后入队，processWrite 合并不得丢 weight——否则 DB 不持久化，
// ≤30s reload 后内存回退，weight 动作被静默撤销。
func TestProcessWriteMergeKeepsWeight(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	// weight 写先入队、status 写后入队（weight=nil）
	s.enqueueWrite(1, accState{status: domain.Status429, errCount: 1}, intPtr(10))
	s.enqueueWrite(1, accState{status: domain.StatusActive}, nil)

	require.NoError(t, s.Close(context.Background())) // 排空触发 processWrite 合并
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Len(t, m.writes, 1, "same-account writes merged into one")
	require.Equal(t, domain.StatusActive, m.writes[0].status, "后写 status 覆盖先写")
	require.NotNil(t, m.writes[0].weight, "后写 weight=nil 不得丢弃已入队的 weight")
	require.Equal(t, 10, *m.writes[0].weight, "最终写必须携带 weight")
}

// TestWorkerContract 满足 worker.Worker 契约（Global Constraints #5）：Name + 幂等 Start。
func TestWorkerContract(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	require.Equal(t, "scheduler", s.Name())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, s.Start(ctx))
	require.EqualError(t, s.Start(ctx), "scheduler: already started")
}

// TestCloseDrainsWritebacks Close 排空 pending 回写且幂等；ctx 超时路径不阻塞。
// 事件经 FlushRules 同步处理后进入回写队列（生产路径由规则引擎 worker 消费）。
func TestCloseDrainsWritebacks(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	s.MarkResult(1, ResultError, nil, 0, "")
	s.FlushRules()                                    // 事件 → apply → 回写入队
	require.NoError(t, s.Close(context.Background())) // 排空 pending 回写
	require.NoError(t, s.Close(context.Background())) // 幂等
	m.mu.Lock()
	require.Len(t, m.writes, 1, "close drains exactly the pending writeback")
	m.mu.Unlock()

	// ctx 已取消：限时路径直接返回（丢弃/尽最大努力），不阻塞。
	s.MarkResult(1, ResultError, nil, 0, "")
	s.FlushRules()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, s.Close(ctx))
}

func mkAcc(id int64, weight int, tpl *domain.Template) *accountSnapshot {
	a := &accountSnapshot{acc: domain.Account{ID: id, Weight: weight}, tpl: tpl}
	a.state.Store(&accState{status: domain.StatusActive})
	return a
}

func tplWith(ff domain.RequestFormat, models []string) *domain.Template {
	return &domain.Template{SupportedFormats: []domain.RequestFormat{ff}, Models: models}
}

func TestNewWeightedSeqGcdNormalization(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	pool := []*accountSnapshot{mkAcc(1, 100, tpl), mkAcc(2, 50, tpl)}
	ws := newWeightedSeq(pool)
	require.Len(t, ws.seq, 3, "weight 100:50 → GCD=50 → 序列长 3")
	count1, count2 := 0, 0
	for _, a := range ws.seq {
		if a.acc.ID == 1 {
			count1++
		} else {
			count2++
		}
	}
	require.Equal(t, 2, count1)
	require.Equal(t, 1, count2)
}

func TestNewWeightedSeqEqualWeights(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	pool := []*accountSnapshot{mkAcc(1, 100, tpl), mkAcc(2, 100, tpl), mkAcc(3, 100, tpl)}
	ws := newWeightedSeq(pool)
	require.Len(t, ws.seq, 3, "全同权重 → 每账号 1 次")
	require.ElementsMatch(t, []int64{1, 2, 3}, []int64{ws.seq[0].acc.ID, ws.seq[1].acc.ID, ws.seq[2].acc.ID})
}

func TestNewWeightedSeqLengthCap(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	// 反例构造超长：权重 9999 与 1 → GCD=1 → 长 10000 > 4096
	pool2 := []*accountSnapshot{mkAcc(1, 9999, tpl), mkAcc(2, 1, tpl)}
	ws := newWeightedSeq(pool2)
	require.LessOrEqual(t, len(ws.seq), maxSeqLen, "长度上限 4096")
	require.Contains(t, []int64{ws.seq[0].acc.ID, ws.seq[1].acc.ID}, int64(1), "权重高的账号至少出现一次")
}

func TestBuildRoutesBucketsAndDefault(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o", "gpt-4o-mini"})
	pool := []*accountSnapshot{mkAcc(1, 100, tpl)}
	routes := buildRoutes(pool)
	// 已知模型桶
	rt, ok := routes[routeKey{domain.FormatOpenAIChat, "gpt-4o"}]
	require.True(t, ok)
	require.NotNil(t, rt.tier1, "gpt-4o 在 models 里 → tier1")
	require.Nil(t, rt.tier2)
	// 默认桶（未知模型回落）
	rtD, ok := routes[routeKey{domain.FormatOpenAIChat, ""}]
	require.True(t, ok)
	require.Nil(t, rtD.tier1)
	require.NotNil(t, rtD.tier2, "未知模型 → 默认格式 tier2")
	// 其他格式无桶
	_, ok = routes[routeKey{domain.FormatAnthropic, "gpt-4o"}]
	require.False(t, ok)
}

func TestBuildRoutesFormatModelsLimit(t *testing.T) {
	tpl := &domain.Template{
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic},
		Models:           []string{"gpt-4o", "special"},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatAnthropic: {"special"}},
	}
	pool := []*accountSnapshot{mkAcc(1, 100, tpl)}
	routes := buildRoutes(pool)
	// anthropic 只支持 special（format_models 限制）→ special 有桶且 tier1（∈ Models → Serves true）
	rt, ok := routes[routeKey{domain.FormatAnthropic, "special"}]
	require.True(t, ok, "FormatModels 配置格式 → special 模型走 anthropic 桶")
	require.NotNil(t, rt.tier1, "special ∈ Models → Serves true → tier1")
	// gpt-4o 不在 anthropic 的 format_models 列表 → 该组合无桶
	_, ok = routes[routeKey{domain.FormatAnthropic, "gpt-4o"}]
	require.False(t, ok, "gpt-4o ∉ FormatModels[anthropic] → 格式不支持该模型")
	// chat 未配置 format_models → 全部模型
	rtC, ok := routes[routeKey{domain.FormatOpenAIChat, "gpt-4o"}]
	require.True(t, ok, "未配置格式 → 全部模型")
	require.NotNil(t, rtC.tier1, "gpt-4o ∈ Models → tier1")
	// responses 不在 supported → 无桶
	_, ok = routes[routeKey{domain.FormatOpenAIResponses, "special"}]
	require.False(t, ok, "格式不在 supported → 无桶")
}

// 分布：10 万次选号，频率 vs 权重比例（±5% 容差，shuffle 后的轮询分布）
func TestSelectWeightDistribution(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k2", Status: domain.StatusActive, Weight: 50, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	const n = 100_000
	counts := map[int64]int{}
	for i := 0; i < n; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		counts[sel.AccountID]++
		s.Release(sel.AccountID)
		s.MarkResult(sel.AccountID, ResultOK, nil, 0, "")
	}
	ratio := float64(counts[1]) / float64(counts[2])
	// 注意：testify 无 InRange，用 InDelta（±0.1 窗口等价于 [1.9, 2.1]）
	require.InDelta(t, ratio, 2.0, 0.1, "weight 100:50 → 频率比 ≈ 2:1")
}

// 动态状态跳过：冷却中的账号被跳过，选中其他账号
func TestSelectSkipsCooldown(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	// 账号 1 进 429 冷却
	s.MarkResult(1, Result429, nil, 0, "")
	s.FlushRules()
	for i := 0; i < 50; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		require.Equal(t, int64(2), sel.AccountID, "冷却中的账号 1 必须被跳过")
		s.Release(sel.AccountID)
	}
}

// 全不可用（全冷却）→ ErrNoAvailable，且有限时间内返回
func TestSelectAllCooldownReturnsNoAvailable(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	s.MarkResult(1, Result429, nil, 0, "")
	s.FlushRules()
	done := make(chan error, 1)
	go func() {
		_, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrNoAvailable)
	case <-time.After(time.Second):
		t.Fatal("全冷却必须有限时间内返回 ErrNoAvailable")
	}
}

// 未知模型回落默认桶：请求 model 不在任何模板可服务集合 → 默认格式 tier2 选中
func TestSelectUnknownModelDefaultBucket(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	sel, err := s.Select(10, domain.FormatOpenAIChat, "unknown-model-xyz")
	require.NoError(t, err, "未知模型走默认回退桶（默认格式 tier2）")
	require.Equal(t, int64(1), sel.AccountID)
}

// tier 回落：tier1 全冷却 → tier2 选中
func TestSelectTierFallback(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 2, Template: &domain.Template{ID: 2, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"other-model"}}, UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	// 账号 1（tier1）进冷却 → 请求 gpt-4o 应回落 tier2（账号 2，Serves 为 false）
	s.MarkResult(1, Result429, nil, 0, "")
	s.FlushRules()
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID, "tier1 全不可用 → tier2 回落")
	s.Release(sel.AccountID)
}

// tier 回落（并发满，Task 2 评审钉死）：tier1 账号并发满 → 回落 tier2（可用性优先）。
// 规范裁定：旧实现（并发满账号在分档前被剔除 → tier1 为空）在此场景直接
// ErrNoAvailable 的语义不可取；新实现 tier1 序列扫描失败后必须回落 tier2。
func TestSelectTier1FullFallsBackToTier2(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1},
		{ID: 2, TemplateID: 2, Template: &domain.Template{ID: 2, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"other-model"}}, UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4},
	})
	require.NoError(t, s.InvalidateAllSync())
	// 账号 1 是唯一 Serves gpt-4o 的账号（tier1 序列只有它）→ 确定性占用其唯一并发槽
	sel1, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel1.AccountID, "tier1 唯一账号先被选中")
	// tier1 并发满 → 必须回落 tier2（账号 2，Serves 恒 false 但同默认格式）
	sel2, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel2.AccountID, "tier1 并发满 → tier2 回落（可用性优先）")
	s.Release(sel1.AccountID)
	s.Release(sel2.AccountID)
}

// 并发 CAS 竞争（Task 2 评审钉死）：单账号（n=1 序列）两并发 Select，
// 恰一成功、另一返回 ErrNoAvailable——单遍单次 CAS 语义（败者不自旋重试，
// 调用方重试）。屏障对齐两 goroutine 后 200 轮放大真实 CAS 冲突。
func TestSelectConcurrentCASRace(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1},
	})
	require.NoError(t, s.InvalidateAllSync())
	type pairResult struct {
		sel *Selection
		err error
	}
	const pairs = 200
	for i := 0; i < pairs; i++ {
		start := make(chan struct{})
		ready := make(chan struct{}, 2)
		results := make(chan pairResult, 2)
		var wg sync.WaitGroup
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready <- struct{}{}
				<-start
				sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
				results <- pairResult{sel: sel, err: err}
			}()
		}
		<-ready
		<-ready
		close(start) // 同时放行，最大化 CAS 读-改-写冲突窗口
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d: pair did not finish in time — CAS loser must return promptly, not spin", i)
		}
		close(results)
		var winner *Selection
		okCount, noAvailCount := 0, 0
		for r := range results {
			switch {
			case r.err == nil:
				okCount++
				winner = r.sel
			case errors.Is(r.err, ErrNoAvailable):
				noAvailCount++
			default:
				t.Fatalf("iter %d: unexpected error: %v", i, r.err)
			}
		}
		require.Equal(t, 1, okCount, "iter %d: exactly one success per pair", i)
		require.Equal(t, 1, noAvailCount, "iter %d: loser gets ErrNoAvailable (never two successes)", i)
		require.NotNil(t, winner, "iter %d: winner carries a selection", i)
		s.Release(winner.AccountID) // 恢复槽位，下一轮从 0 并发开始
	}
}
