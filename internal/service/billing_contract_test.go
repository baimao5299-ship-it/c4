package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
)

// 本文件覆盖 Phase 5 T4 service 层契约校验：用户/组倍率范围、service_tier
// policy 设置值域、用户余额负值。

// intPtr *int 构造（service 包既有 ptr 为 string 专用）。
func intPtr(v int) *int { return &v }

func TestUserMultiplierValidation(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, invalidate: func() {}, log: nil}
	ctx := context.Background()

	// 创建：nil / 0 / 100000 合法（email 唯一，逐次不同）
	for i, mult := range []*int{nil, intPtr(0), intPtr(100000)} {
		u, err := svc.CreateUser(ctx, fmt.Sprintf("m%d@example.com", i), "s3cret-pass",
			domain.RoleUser, domain.UserStatusActive, 0, 0, mult)
		require.NoError(t, err, "mult=%v", mult)
		if mult == nil {
			require.Nil(t, u.PriceMultiplier)
		} else {
			require.NotNil(t, u.PriceMultiplier)
			require.Equal(t, *mult, *u.PriceMultiplier)
		}
	}
	// 超界 → 400
	for _, mult := range []int{-1, 100001} {
		_, err := svc.CreateUser(ctx, "x@example.com", "s3cret-pass",
			domain.RoleUser, domain.UserStatusActive, 0, 0, intPtr(mult))
		require.ErrorIs(t, err, ErrInvalidInput, "mult=%d", mult)
	}

	// 更新：合法值/超界
	u, err := svc.GetUser(ctx, 1)
	require.NoError(t, err)
	u.PriceMultiplier = intPtr(5000)
	_, err = svc.UpdateUser(ctx, u)
	require.NoError(t, err)
	u.PriceMultiplier = intPtr(100001)
	_, err = svc.UpdateUser(ctx, u)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestGroupMultiplierValidation(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, invalidate: func() {}, log: nil}
	ctx := context.Background()

	// 创建：0 = 未指定（fake 落 10000）；20000 显式
	g, err := svc.CreateGroup(ctx, "g0", domain.GroupVisibilityPublic, 0)
	require.NoError(t, err)
	require.Equal(t, 10000, g.PriceMultiplier, "0 = 未指定 → 组默认 ×1")
	g, err = svc.CreateGroup(ctx, "g1", domain.GroupVisibilityPublic, 20000)
	require.NoError(t, err)
	require.Equal(t, 20000, g.PriceMultiplier)

	// 创建/更新超界 → 400
	_, err = svc.CreateGroup(ctx, "g2", domain.GroupVisibilityPublic, -1)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.CreateGroup(ctx, "g3", domain.GroupVisibilityPublic, 100001)
	require.ErrorIs(t, err, ErrInvalidInput)
	g.PriceMultiplier = 100001
	_, err = svc.UpdateGroup(ctx, g)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestServiceTierPolicySettingValidation(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, invalidate: func() {}, log: nil}
	ctx := context.Background()

	// 三值均可
	for _, v := range []string{"passthrough", "strip", "reject"} {
		_, err := svc.UpdateSetting(ctx, "service_tier_policy_priority", v)
		require.NoError(t, err, "value=%s", v)
		_, err = svc.UpdateSetting(ctx, "service_tier_policy_flex", v)
		require.NoError(t, err, "value=%s", v)
	}
	// 非法 → 400
	for _, key := range []string{"service_tier_policy_priority", "service_tier_policy_flex"} {
		_, err := svc.UpdateSetting(ctx, key, "bogus")
		require.ErrorIs(t, err, ErrInvalidInput, "key=%s", key)
	}
	// 未知 key → 400（注册表）
	_, err := svc.UpdateSetting(ctx, "service_tier_policy_bogus", "passthrough")
	require.ErrorIs(t, err, ErrInvalidInput)
}
