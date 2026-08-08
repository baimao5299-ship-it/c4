package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeBalLoader 余额 + 倍率快照测试 loader（failLoad 注入失败——fail-safe 断言）。
type fakeBalLoader struct {
	m      map[int64]int64 // 余额
	um     map[int64]int   // 用户倍率（仅已设置行）
	gm     map[int64]int   // 组倍率
	failAt int             // 1 = LoadBalances 失败；2 = LoadGroupMultipliers 失败
}

func (f fakeBalLoader) LoadBalances(ctx context.Context) (map[int64]int64, map[int64]int, error) {
	if f.failAt == 1 {
		return nil, nil, errors.New("db down")
	}
	return f.m, f.um, nil
}

func (f fakeBalLoader) LoadGroupMultipliers(ctx context.Context) (map[int64]int, error) {
	if f.failAt == 2 {
		return nil, errors.New("db down")
	}
	return f.gm, nil
}

// TestBalancesReloadFailSafe Reload 失败 fail-safe：Warn（log 注入）+ 保留旧
// 快照不替换（预检继续用旧值，条件扣 DB 兜底）；空初始快照 → 全部缺失。
func TestBalancesReloadFailSafe(t *testing.T) {
	// 先成功加载 → 再失败 → 旧快照保留
	b := NewBalances(fakeBalLoader{m: map[int64]int64{3: 77}}, nil)
	require.NoError(t, b.Reload(context.Background()))
	b.loader = fakeBalLoader{failAt: 1}
	require.Error(t, b.Reload(context.Background()))
	bal, ok := b.BalanceOf(3)
	require.True(t, ok)
	require.Equal(t, int64(77), bal, "Reload 失败保留旧快照")

	// 初始失败（空快照）→ 全部缺失（预检 402 拒绝，安全侧）
	b2 := NewBalances(fakeBalLoader{failAt: 1}, nil)
	require.Error(t, b2.Reload(context.Background()))
	_, ok = b2.BalanceOf(1)
	require.False(t, ok)
}

// TestBalancesReloadGroupFailSafe 组倍率加载失败 → 余额与倍率两路都保留旧值
//（快照内自洽：不出现新余额 + 旧倍率错配）。
func TestBalancesReloadGroupFailSafe(t *testing.T) {
	b := NewBalances(fakeBalLoader{
		m: map[int64]int64{1: 100}, um: map[int64]int{1: 20000}, gm: map[int64]int{1: 15000},
	}, nil)
	require.NoError(t, b.Reload(context.Background()))
	// 余额加载成功、组倍率失败 → 整体不替换
	b.loader = fakeBalLoader{m: map[int64]int64{1: 999}, um: map[int64]int{1: 20000}, failAt: 2}
	require.Error(t, b.Reload(context.Background()))
	bal, ok := b.BalanceOf(1)
	require.True(t, ok)
	require.Equal(t, int64(100), bal, "组倍率失败 → 余额也保留旧值")
	require.Equal(t, 15000, b.EffectiveMultiplier(2, 1), "组倍率保留旧值")
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

// TestEffectiveMultiplier 有效倍率表驱动（T3.5 用户覆盖组语义）：用户已设置
// （含 0 免费）→ 覆盖组倍率；仅组 → 组倍率；均缺 → 10000。
func TestEffectiveMultiplier(t *testing.T) {
	cases := []struct {
		name        string
		users       map[int64]int
		groups      map[int64]int
		userID, gid int64
		want        int
	}{
		{"用户覆盖组", map[int64]int{1: 20000}, map[int64]int{1: 15000}, 1, 1, 20000},
		{"用户免费覆盖组", map[int64]int{1: 0}, map[int64]int{1: 15000}, 1, 1, 0},
		{"仅组倍率", nil, map[int64]int{1: 15000}, 1, 1, 15000},
		{"均缺默认×1", nil, nil, 1, 1, 10000},
		{"用户×10 上限", map[int64]int{1: 100000}, nil, 1, 1, 100000},
		{"无组无用户", nil, nil, 99, 0, 10000},
		{"用户未设置用组", nil, map[int64]int{2: 0}, 1, 2, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := NewBalances(fakeBalLoader{m: map[int64]int64{}, um: c.users, gm: c.groups}, nil)
			require.NoError(t, b.Reload(context.Background()))
			require.Equal(t, c.want, b.EffectiveMultiplier(c.userID, c.gid))
		})
	}
	// 未 Reload（倍率快照空）→ 默认 ×1
	b := NewBalances(fakeBalLoader{m: map[int64]int64{}}, nil)
	require.Equal(t, 10000, b.EffectiveMultiplier(1, 1))
}
