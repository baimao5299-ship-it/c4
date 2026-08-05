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
	require.Equal(t, domain.StatusErr, ri.Status)
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
