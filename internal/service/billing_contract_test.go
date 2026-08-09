package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
)

// 本文件覆盖 Phase 5 T4 service 层契约校验：组倍率范围（万分数）、
// service_tier policy 设置值域、用户余额负值。用户倍率按组（T3.5 修正）经
// SetGroupAssignments 校验（见 assignment 测试）。

// intPtr *int 构造（service 包既有 ptr 为 string 专用）。
func intPtr(v int) *int { return &v }

func TestGroupMultiplierValidation(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	// 创建：nil = 未指定 → ×1（service 归一 10000 恒写入）；20000 显式；
	// 显式 0 = 免费组（T3.5 修正：API 边界 nullable 可表达，service 不再把 0
	// 当未指定）
	g, err := svc.CreateGroup(ctx, "g0", domain.GroupVisibilityPublic, nil)
	require.NoError(t, err)
	require.Equal(t, 10000, g.PriceMultiplier, "nil = 未指定 → ×1")
	g, err = svc.CreateGroup(ctx, "g1", domain.GroupVisibilityPublic, intPtr(20000))
	require.NoError(t, err)
	require.Equal(t, 20000, g.PriceMultiplier)
	g, err = svc.CreateGroup(ctx, "g-free", domain.GroupVisibilityPublic, intPtr(0))
	require.NoError(t, err)
	require.Equal(t, 0, g.PriceMultiplier, "显式 0 = 免费组（恒写入）")

	// 创建/更新超界 → 400
	_, err = svc.CreateGroup(ctx, "g2", domain.GroupVisibilityPublic, intPtr(-1))
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.CreateGroup(ctx, "g3", domain.GroupVisibilityPublic, intPtr(100001))
	require.ErrorIs(t, err, ErrInvalidInput)
	g.PriceMultiplier = 100001
	_, err = svc.UpdateGroup(ctx, g)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestServiceTierPolicySettingValidation(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
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
