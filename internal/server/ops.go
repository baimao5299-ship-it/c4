// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// GET /ops/workers 管理面端点（spec 2026-08-11）：按需聚合各 worker 可观测
// 状态。采集纪律：原子读既有计数器 + len(channel)（零锁零分配，O(1)）；不
// 做持续采集/推送/存储；admin 鉴权（与 /admin 同中间件）。
//
// StatsProvider 是独立契约（不改 worker.Worker——Manager 无枚举）：各 worker
// 模块加 Stats() any（原子读组装，不锁模块内部），装配侧类型断言逐个入列；
// 响应 typed struct 非 map（便于验收断言）。

// StatsProvider worker 可观测状态提供面。
type StatsProvider interface {
	Name() string
	Stats() any
}

// WorkerStatus 单个 worker 的可观测状态条目。
type WorkerStatus struct {
	Name  string `json:"name"`
	Stats any    `json:"stats"`
}

// SnapshotState 快照注册表状态（ops 响应专用映射——snapshot.Status 的
// LastError 是 error 接口，JSON 序列化不可用）。
type SnapshotState struct {
	Name       string    `json:"name"`
	Scopes     []string  `json:"scopes,omitempty"`
	LastReload time.Time `json:"last_reload"`
	LastError  string    `json:"last_error,omitempty"`
}

// WorkersResponse GET /ops/workers 响应（typed struct，非 map[string]any）。
type WorkersResponse struct {
	Workers     []WorkerStatus  `json:"workers"`
	Snapshots   []SnapshotState `json:"snapshots"`
	GeneratedAt time.Time       `json:"generated_at"`
}

// OpsOptions GET /ops/workers 装配参数。
type OpsOptions struct {
	Workers   []StatsProvider
	Snapshots func() []SnapshotState // nil = 快照区返回空
}

// adminAuth /admin 与 /ops 共用的管理面鉴权 = 静态 admin token OR platform_admin
// JWT（两个都过才拒）。JWT 路径同样做快照用户状态校验（禁用即拒；评审定夺②）。
// 从 NewServer 内联闭包抽出（spec 2026-08-11：/ops 需同鉴权挂载）。
func adminAuth(opts Options) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			authz := req.Header.Get("Authorization")
			if authz == "Bearer "+opts.AdminToken {
				// 静态 admin token 路径不注入 UserID（决策 5：handler 读到 0 = 系统）
				next.ServeHTTP(w, req)
				return
			}
			if opts.JWTIssuer != nil && strings.HasPrefix(authz, "Bearer ") {
				claims, err := opts.JWTIssuer.Verify(strings.TrimPrefix(authz, "Bearer "))
				if err == nil && claims.Role == string(domain.RolePlatformAdmin) {
					active := false
					if opts.UserStatus == nil {
						active = true
					} else if st, ok := opts.UserStatus.UserStatus(claims.UserID); ok && st == domain.UserStatusActive {
						active = true
					}
					if active {
						// JWT 路径注入 claims.UserID（兑换码 created_by 用，决策 5）
						ctx := context.WithValue(req.Context(), adminUserIDKey{}, claims.UserID)
						next.ServeHTTP(w, req.WithContext(ctx))
						return
					}
				}
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		})
	}
}

// NewOpsHandler 构造 /ops/workers 处理器（按需组装，无缓存）。
func NewOpsHandler(opts OpsOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := make([]WorkerStatus, 0, len(opts.Workers))
		for _, p := range opts.Workers {
			ws = append(ws, WorkerStatus{Name: p.Name(), Stats: p.Stats()})
		}
		var snaps []SnapshotState
		if opts.Snapshots != nil {
			snaps = opts.Snapshots()
		} else {
			snaps = []SnapshotState{} // 未装配快照区 → JSON [] 非 null
		}
		writeJSON(w, http.StatusOK, WorkersResponse{
			Workers:     ws,
			Snapshots:   snaps,
			GeneratedAt: time.Now().UTC(),
		})
	})
}
