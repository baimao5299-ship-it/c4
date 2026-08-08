package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeBalLoader struct {
	m   map[int64]int64
	err error
}

func (f fakeBalLoader) LoadBalances(ctx context.Context) (map[int64]int64, error) {
	return f.m, f.err
}

// TestBalancesReloadFailSafe Reload 失败 fail-safe：Warn（log 注入）+ 保留旧
// 快照不替换（预检继续用旧值，条件扣 DB 兜底）；空初始快照 → 全部缺失。
func TestBalancesReloadFailSafe(t *testing.T) {
	// 先成功加载 → 再失败 → 旧快照保留
	b := NewBalances(fakeBalLoader{m: map[int64]int64{3: 77}}, nil)
	require.NoError(t, b.Reload(context.Background()))
	b.loader = fakeBalLoader{err: errors.New("db down")}
	require.Error(t, b.Reload(context.Background()))
	bal, ok := b.BalanceOf(3)
	require.True(t, ok)
	require.Equal(t, int64(77), bal, "Reload 失败保留旧快照")

	// 初始失败（空快照）→ 全部缺失（预检 402 拒绝，安全侧）
	b2 := NewBalances(fakeBalLoader{err: errors.New("db down")}, nil)
	require.Error(t, b2.Reload(context.Background()))
	_, ok = b2.BalanceOf(1)
	require.False(t, ok)
}

// TestBalancesSet 扣费后定向刷新：目标用户更新，其余用户与新增用户不受影响。
func TestBalancesSet(t *testing.T) {
	b := NewBalances(fakeBalLoader{m: map[int64]int64{1: 100, 2: 200}}, nil)
	require.NoError(t, b.Reload(context.Background()))
	b.Set(1, 40)
	bal, ok := b.BalanceOf(1)
	require.True(t, ok)
	require.Equal(t, int64(40), bal, "Set 后命中新值")
	bal, ok = b.BalanceOf(2)
	require.True(t, ok)
	require.Equal(t, int64(200), bal, "其余用户不受影响")
	b.Set(9, 1) // 新用户（快照缺失）补入
	bal, ok = b.BalanceOf(9)
	require.True(t, ok)
	require.Equal(t, int64(1), bal)
}

// TestBalancesBalanceOfMissing 缺失 → (0, false)（预检 402 语义：无快照 =
// 拒绝，不按 0 放行）。
func TestBalancesBalanceOfMissing(t *testing.T) {
	b := NewBalances(fakeBalLoader{m: map[int64]int64{}}, nil)
	require.NoError(t, b.Reload(context.Background()))
	_, ok := b.BalanceOf(42)
	require.False(t, ok)
	bal, ok := b.BalanceOf(1)
	require.False(t, ok)
	require.Zero(t, bal)
}
