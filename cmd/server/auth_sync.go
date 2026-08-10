package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go-proxy-mini/internal/invalidate"
	"go-proxy-mini/pkg/logx"
)

// authSyncInterval 鉴权快照周期兜底（设计文档 §1.6/§5 #9 / R1）：NOTIFY 丢失/
// 断连期间，key CRUD 与用户变更最长 60s 收敛。现状 auth 无周期 reload——这是
// 60s 兜底缺口的半侧，本 worker 补位。
const authSyncInterval = 60 * time.Second

// authSync 周期全量刷新鉴权快照 worker（worker.Worker 契约，Name="auth-sync"）：
// 每 interval 调一次 Auth.Reload。与 invalidate 的 users/keys 事件驱动分支并存
// （事件即时 + 周期兜底双保险）；Reload 内部持锁整体换快照（last-wins），与
// 事件驱动并发调用安全。
type authSync struct {
	auth      invalidate.AuthReloader
	interval  time.Duration
	log       *logx.Logger
	startOnce atomic.Bool
}

// newAuthSync 构造周期 worker；interval <= 0 → authSyncInterval（60s）。
func newAuthSync(auth invalidate.AuthReloader, interval time.Duration, log *logx.Logger) *authSync {
	if interval <= 0 {
		interval = authSyncInterval
	}
	return &authSync{auth: auth, interval: interval, log: log}
}

// Name 满足 worker.Worker 契约。
func (w *authSync) Name() string { return "auth-sync" }

// Start 启动周期 reload goroutine（幂等：重复 Start 返回错误；worker 契约）。
func (w *authSync) Start(ctx context.Context) error {
	if !w.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("auth-sync: already started")
	}
	go func() {
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := w.auth.Reload(ctx); err != nil && w.log != nil {
					w.log.Warn("auth periodic reload failed", logx.Error(err))
				}
			}
		}
	}()
	return nil
}

// Close 无操作：循环随 Start 的 ctx 取消退出（worker 契约：幂等、未 Start 也
// 安全）。
func (w *authSync) Close(ctx context.Context) error { return nil }
