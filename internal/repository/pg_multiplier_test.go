package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
)

// TestPGGroupMultiplierDefault 组倍率默认 10000（T3.5）：Create 未指定（0 =
// 未设置，不落列）→ DB 默认 ×1；LoadGroupMultipliers 全量读出。
func TestPGGroupMultiplierDefault(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "def-mult", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	require.Equal(t, 10000, g.PriceMultiplier, "Create 未指定 → DB 默认 10000")

	got, err := repos.Groups.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, 10000, got.PriceMultiplier, "roundtrip：读回默认倍率")

	mults, err := repos.Groups.LoadGroupMultipliers(ctx)
	require.NoError(t, err)
	require.Equal(t, 10000, mults[g.ID], "快照含该组默认倍率")
}

// TestPGGroupMultiplierSetUpdate 组倍率显式设置 + 更新（T3.5）：Create 设
// 15000 → 读回；Update 显式 0 = 免费组 → 读回 0；再改 20000 → 读回。
func TestPGGroupMultiplierSetUpdate(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "mult-g", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 15000,
	})
	require.NoError(t, err)
	require.Equal(t, 15000, g.PriceMultiplier, "Create 显式设倍率")

	got, err := repos.Groups.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, 15000, got.PriceMultiplier, "roundtrip：显式倍率读回")

	// Update 恒写入：显式 0 = 免费组
	g.PriceMultiplier = 0
	updated, err := repos.Groups.UpdateGroup(ctx, g)
	require.NoError(t, err)
	require.Equal(t, 0, updated.PriceMultiplier, "Update 显式 0 = 免费组")

	g.PriceMultiplier = 20000
	updated, err = repos.Groups.UpdateGroup(ctx, g)
	require.NoError(t, err)
	require.Equal(t, 20000, updated.PriceMultiplier, "Update 改倍率")
}

// TestPGUserMultiplierNilSetClear 用户倍率 nil/设值/清空 roundtrip（T3.5
// SetNillable 语义）：Create nil = 未设置；Update 设 15000 → 读回；Update nil
// 清除为未设置 → 读回 nil。
func TestPGUserMultiplierNilSetClear(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "mult@example.com")
	require.Nil(t, u.PriceMultiplier, "Create 未设置 → nil")

	u.PriceMultiplier = ptr(15000)
	set, err := repos.Users.UpdateUser(ctx, u)
	require.NoError(t, err)
	require.NotNil(t, set.PriceMultiplier)
	require.Equal(t, 15000, *set.PriceMultiplier, "Update 设值读回")

	u.PriceMultiplier = nil
	cleared, err := repos.Users.UpdateUser(ctx, u)
	require.NoError(t, err)
	require.Nil(t, cleared.PriceMultiplier, "Update nil 清除为未设置")
}

// TestPGLoadBalancesWithMultipliers LoadBalances 扩展（T3.5）：同时返回余额
// map 与用户倍率 map——仅 price_multiplier 非 NULL 行进入倍率 map（存在 =
// 已设置；缺失 = 未设置 → 用组倍率）。
func TestPGLoadBalancesWithMultipliers(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u1 := seedPGUser(t, repos, "lb1@example.com")
	u2 := seedPGUser(t, repos, "lb2@example.com")
	// 先设倍率（UpdateUser 全量替换语义会写 balance），再原子加余额——互不干扰。
	u2.PriceMultiplier = ptr(0) // 免费用户（0 是合法值，必须入 map）
	_, err := repos.Users.UpdateUser(ctx, u2)
	require.NoError(t, err)
	require.NoError(t, repos.UpdateUserBalance(ctx, u1.ID, 50000))
	require.NoError(t, repos.UpdateUserBalance(ctx, u2.ID, 70000))

	bals, mults, err := repos.LoadBalances(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(50000), bals[u1.ID])
	require.Equal(t, int64(70000), bals[u2.ID])
	_, has := mults[u1.ID]
	require.False(t, has, "未设置倍率的用户不入 map（缺失 = 用组倍率）")
	require.Equal(t, 0, mults[u2.ID], "倍率 0（免费）是已设置值，必须入 map")
}

func ptr(v int) *int { return &v }
