package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
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

func (m *memLoader) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldown *time.Time, lastErr *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes = append(m.writes, statusWrite{id: id, status: status, cooldown: cooldown})
	return nil
}

func testCfg() Config {
	return Config{
		DefaultMaxConcurrency: 2,
		Cooldown429:           30 * time.Second,
		BackoffBase:           5 * time.Second,
		BackoffMax:            5 * time.Minute,
		SyncInterval:          100 * time.Hour, // 测试中不触发定时同步
	}
}

func tpl(id int64, format domain.RequestFormat, models []string) *domain.Template {
	return &domain.Template{ID: id, BaseURL: "https://u/v1", DefaultFormat: format, Models: models}
}

func acc(id int64, t *domain.Template, maxConc int) *domain.Account {
	return &domain.Account{ID: id, TemplateID: t.ID, Template: t, UpstreamKey: "k", Status: domain.StatusActive, Weight: 100, MaxConcurrency: maxConc}
}

func newSched(t *testing.T, m *memLoader) *Scheduler {
	t.Helper()
	s := New(testCfg(), m, nil)
	require.NoError(t, s.reload(context.Background()))
	return s
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

	reset := time.Now().Add(10 * time.Second)
	s.MarkResult(1, Result429, &reset)
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "in cooldown should be unavailable")
	// 冷却过期后惰性恢复
	s.timeNow = func() time.Time { return time.Now().Add(15 * time.Second) }
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "available after cooldown")
	s.MarkResult(sel.AccountID, ResultOK, nil)
	s.Release(sel.AccountID)
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

	s.MarkResult(1, ResultError, nil) // 第一次失败 → backoff base
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status)
	require.Equal(t, 1, ri.ErrCount)
	require.NotNil(t, ri.CooldownUntil)
	require.True(t, ri.CooldownUntil.After(time.Now().Add(4*time.Second)), "backoff base applied")
	s.MarkResult(1, ResultError, nil) // 第二次 → 指数
	ri, _ = s.Runtime(1)
	require.Equal(t, 2, ri.ErrCount)
	s.MarkResult(1, ResultOK, nil)
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
	s.MarkResult(3, ResultError, nil)
	ri, _ = s.Runtime(3)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "markresult hits the new snapshot")
	require.Equal(t, 1, ri.ErrCount)

	// 被移除账号 1：Runtime 不可见，MarkResult/Release 安全 no-op（无回写）。
	_, ok = s.Runtime(1)
	require.False(t, ok, "removed account must not be in byID")
	s.MarkResult(1, ResultError, nil)
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
	s.MarkResult(5, ResultError, nil)
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

	// 在途请求完成：OK 不得把状态重置回 active、不得重置错误计数
	s.MarkResult(1, ResultOK, nil)
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusDisabled, ri.Status, "OK 不得复活禁用账号")
	require.Zero(t, ri.ErrCount, "OK 不得重置禁用账号的错误计数")

	// 防御性：429/错误分支同样不得给禁用账号设置冷却或改写状态
	s.MarkResult(1, Result429, nil)
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusDisabled, ri.Status, "429 不得把禁用账号改写为 429")
	require.Nil(t, ri.CooldownUntil, "429 不得给禁用账号设置冷却")
	s.MarkResult(1, ResultError, nil)
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
func TestCloseDrainsWritebacks(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	s.MarkResult(1, ResultError, nil)
	require.NoError(t, s.Close(context.Background())) // 排空 pending 回写
	require.NoError(t, s.Close(context.Background())) // 幂等
	m.mu.Lock()
	require.Len(t, m.writes, 1, "close drains exactly the pending writeback")
	m.mu.Unlock()

	// ctx 已取消：限时路径直接返回（丢弃/尽最大努力），不阻塞。
	s.MarkResult(1, ResultError, nil)
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
	return &domain.Template{DefaultFormat: ff, Models: models}
}

func TestNewWeightedSeqGcdNormalization(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	pool := []*accountSnapshot{mkAcc(1, 100, tpl), mkAcc(2, 50, tpl)}
	ws := newWeightedSeq(pool)
	require.Len(t, ws.seq, 3, "weight 100:50 → GCD=50 → 序列长 3")
	count1, count2 := 0, 0
	for _, a := range ws.seq {
		if a.acc.ID == 1 { count1++ } else { count2++ }
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

func TestBuildRoutesModelFormatsOverride(t *testing.T) {
	tpl := &domain.Template{DefaultFormat: domain.FormatOpenAIChat, ModelFormats: map[string]domain.RequestFormat{"special": domain.FormatAnthropic}}
	pool := []*accountSnapshot{mkAcc(1, 100, tpl)}
	routes := buildRoutes(pool)
	rt, ok := routes[routeKey{domain.FormatAnthropic, "special"}]
	require.True(t, ok, "ModelFormats 覆盖 → special 模型走 anthropic 格式")
	require.NotNil(t, rt.tier1, "special ∈ ModelFormats keys → Serves true → tier1")
}
