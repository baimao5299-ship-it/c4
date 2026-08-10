package proxy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// N=1 单实例等价回归：本地限额 = rpm（与现状一致）。
func TestLimitN1Equivalent(t *testing.T) {
	l := newFixedWindowLimiter(10)
	now := time.Now()
	for i := 0; i < 10; i++ {
		require.True(t, l.Allow(1, now), "第 %d 个请求（N=1 限额 10）", i+1)
	}
	require.False(t, l.Allow(1, now), "第 11 个请求超限")
	require.True(t, l.Allow(2, now), "不同分组独立窗口")
}

// N 分摊：本地限额 = ceil(rpm/N)（§3.2）；窗口重置恢复。
func TestLimitSplitByN(t *testing.T) {
	l := newFixedWindowLimiter(10)
	l.SetInstancesProvider(fakeInstances(3)) // ceil(10/3) = 4
	now := time.Now()
	for i := 0; i < 4; i++ {
		require.True(t, l.Allow(1, now), "N=3 本地限额 4：第 %d 个", i+1)
	}
	require.False(t, l.Allow(1, now), "第 5 个超限（本地窗口）")
	require.False(t, l.Allow(1, now.Add(30*time.Second)), "窗口未重置仍超限")

	// 窗口重置：下个窗口恢复本地限额 4
	winStart := now.Add(time.Minute)
	require.True(t, l.Allow(1, winStart), "新窗口重置")
	for i := 0; i < 3; i++ {
		require.True(t, l.Allow(1, winStart.Add(time.Duration(i)*time.Second)), "新窗口第 %d 个", i+2)
	}
	require.False(t, l.Allow(1, winStart.Add(4*time.Second)), "新窗口仍限额 4")
}

// N 变更：下次窗口生效（§3.4 瞬态可接受）。
func TestLimitNChangeNextWindow(t *testing.T) {
	l := newFixedWindowLimiter(6)
	l.SetInstancesProvider(fakeInstances(2)) // ceil(6/2) = 3
	now := time.Now()
	for i := 0; i < 3; i++ {
		require.True(t, l.Allow(1, now))
	}
	require.False(t, l.Allow(1, now))

	// N 3 → 6：当前窗口仍按旧 N（偏放行，安全侧）
	require.False(t, l.Allow(1, now.Add(30*time.Second)), "当前窗口按旧 N 限额 3")
	l.SetInstancesProvider(fakeInstances(6)) // ceil(6/6) = 1
	require.True(t, l.Allow(1, now.Add(time.Minute)), "新窗口按新 N 限额 1")
	require.False(t, l.Allow(1, now.Add(time.Minute).Add(time.Second)), "新窗口仍限额 1")
}

// 未装配 N（nil provider）与 rpm=0 关闭：单实例等价。
func TestLimitNilProviderAndDisabled(t *testing.T) {
	l := newFixedWindowLimiter(5)
	now := time.Now()
	for i := 0; i < 5; i++ {
		require.True(t, l.Allow(1, now), "nil provider → N=1 等价")
	}
	require.False(t, l.Allow(1, now))

	disabled := newFixedWindowLimiter(0)
	require.True(t, disabled.Allow(1, time.Now()), "rpm=0 限流关闭")
	require.True(t, disabled.Allow(1, time.Now()))
}
