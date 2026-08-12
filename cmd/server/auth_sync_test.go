// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recAuthN 记录 Reload 调用次数（auth-sync 周期测试目标）。
type recAuthN struct {
	mu sync.Mutex
	n  int
}

func (r *recAuthN) Reload(ctx context.Context) error { r.mu.Lock(); r.n++; r.mu.Unlock(); return nil }
func (r *recAuthN) calls() int                       { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// TestAuthSyncPeriodicReload 短周期（5ms）实测：周期到点调用 Auth.Reload
// （≥2 次）；Close 后不再增长。
func TestAuthSyncPeriodicReload(t *testing.T) {
	auth := &recAuthN{}
	w := newAuthSync(auth, 5*time.Millisecond, nil)
	require.Equal(t, "auth-sync", w.Name())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, w.Start(ctx))
	require.Error(t, w.Start(ctx), "重复 Start 返回错误（worker 契约）")

	require.Eventually(t, func() bool { return auth.calls() >= 2 }, time.Second, 10*time.Millisecond,
		"周期到点应触发 ≥2 次 Reload")

	require.NoError(t, w.Close(ctx))
	// 循环随 Start ctx 取消退出（Close no-op，与 invalidate/scheduler 同契约）
	cancel()
	time.Sleep(50 * time.Millisecond) // 等循环退出（select 上 ctx.Done 立即返回）
	before := auth.calls()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, before, auth.calls(), "ctx 取消后循环退出，不再 reload")
}

// TestAuthSyncNotStartedCloseSafe 未 Start 的 Close 安全（worker 契约）。
func TestAuthSyncNotStartedCloseSafe(t *testing.T) {
	w := newAuthSync(&recAuthN{}, 0, nil) // interval 0 → 默认 60s
	require.NoError(t, w.Close(context.Background()))
}
