// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// persistLoader 模拟 DB 持久化的 Loader：UpdateAccountStatus 把状态写回数据源
// （重启快照重载 = 从数据源重建快照仍摘除——memLoader 只记录不落数据，无法
// 表达"重启"语义）。
type persistLoader struct {
	mu      sync.Mutex
	byGroup map[int64][]*domain.Account
}

func newPersistLoader(byGroup map[int64][]*domain.Account) *persistLoader {
	return &persistLoader{byGroup: byGroup}
}

func (m *persistLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64][]*domain.Account, len(m.byGroup))
	for k, v := range m.byGroup {
		cp := make([]*domain.Account, len(v))
		for i, a := range v {
			ac := *a // 值副本：快照与数据源互不干扰
			cp[i] = &ac
		}
		out[k] = cp
	}
	return out, nil
}

func (m *persistLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*domain.Account, len(m.byGroup[id]))
	for i, a := range m.byGroup[id] {
		ac := *a
		out[i] = &ac
	}
	return out, nil
}

func (m *persistLoader) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldown *time.Time, lastErr *string, weight *int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, accs := range m.byGroup {
		for _, a := range accs {
			if a.ID == id {
				a.Status = status
				a.CooldownUntil = cooldown
				a.LastError = lastErr
				return nil
			}
		}
	}
	return nil
}

// drainWrites 排空回写队列（等价 Close 的排空逻辑；测试并发无写回循环时使用）。
func drainWrites(t *testing.T, s *Scheduler) {
	t.Helper()
	require.NoError(t, s.Close(context.Background()))
}

// newSchedLoader 同 newSched，接受任意 Loader（memLoader / persistLoader）。
func newSchedLoader(t *testing.T, m Loader) *Scheduler {
	t.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))
	return s
}

// TestFailAccountRemovesFromSelection 失效摘除：快照置 disabled 后 pickFrom
// 复用既有过滤器跳过（Select → ErrNoAvailable）。
func TestFailAccountRemovesFromSelection(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)

	s.FailAccount(1, "auth permanently revoked")

	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "失效账号不得再被选中")

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status)
}

// TestFailAccountPersistsAcrossReload 摘除必须持久化（brief 明示：仅内存摘除则
// 重启后快照重载复活——pickFrom 不查 failed_at，必须落库 status=disabled）：
// FailAccount → 回写 drain（loader 落库）→ 全量重载（模拟重启快照重建）→ 仍摘除。
func TestFailAccountPersistsAcrossReload(t *testing.T) {
	pl := newPersistLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSchedLoader(t, pl)

	s.FailAccount(1, "auth permanently revoked")
	drainWrites(t, s) // 回写经 loader 落库（status=disabled + last_error）

	pl.mu.Lock()
	require.Equal(t, domain.StatusDisabled, pl.byGroup[10][0].Status, "loader 落库 status=disabled")
	pl.mu.Unlock()

	require.NoError(t, s.InvalidateAllSync()) // 重启等价：从数据源全量重建快照
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "重启快照重载后仍摘除")
}

// TestFailAccountAuditsLastError 审计 last_error：回写携带失效原因摘要
// （域内截断 500——与 last_error 列共用截断语义）。
func TestFailAccountAuditsLastError(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	long := strings.Repeat("x", 600)
	s.FailAccount(1, long)
	drainWrites(t, s)

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Len(t, m.writes, 1)
	require.Equal(t, domain.StatusDisabled, m.writes[0].status)
	require.NotNil(t, m.writes[0].lastErr)
	require.Equal(t, domain.ErrMsgMaxLen, len(*m.writes[0].lastErr), "last_error 域内截断 500")
}

// TestFailAccountMarkResultGuard 防复活守卫复用：失效后 MarkResult（成功/错误）
// 不得把状态重置回 active 并回写 DB（MarkResult 对 disabled 短路）。
func TestFailAccountMarkResultGuard(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	// 在途请求占槽后失效（模拟：失效时仍有在途请求完成回流）
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)

	s.FailAccount(1, "auth permanently revoked")
	s.MarkResult(1, ResultOK, nil, 200, "")
	s.MarkResult(1, ResultError, nil, 0, "stale error")
	s.FlushRules()
	s.Release(1)
	drainWrites(t, s)

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Len(t, m.writes, 1, "仅失效摘除一次回写；MarkResult 全部短路")
	require.Equal(t, domain.StatusDisabled, m.writes[0].status)

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "快照保持 disabled")
}

// TestFailAccountUnknownAccount 快照外/未加载账号：no-op 不 panic。
func TestFailAccountUnknownAccount(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	s.FailAccount(999, "unknown") // 快照外账号
	s.FailAccount(1, "known")     // 正常路径不破坏
	drainWrites(t, s)

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Len(t, m.writes, 1)
	require.Equal(t, int64(1), m.writes[0].id)
}
