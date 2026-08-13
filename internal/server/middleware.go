// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/is7qin/c3api/pkg/logx"
)

// adminUserIDKey /admin 认证中间件写入的 platform_admin 用户 id（created_by 用，
// 决策 5：0 = 系统）。静态 admin token 路径不写入（handler 读到 0）。
type adminUserIDKey struct{}

// UserIDFromContext 取 /admin JWT 鉴权路径注入的用户 id（兑换码生成 created_by
// 用）；静态 admin token 路径未注入 → (0, false)。
func UserIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(adminUserIDKey{}).(int64)
	return id, ok
}

type inflightCounter struct{ v atomic.Int64 }

func (c *inflightCounter) Inc() int64  { return c.v.Add(1) }
func (c *inflightCounter) Dec()        { c.v.Add(-1) }
func (c *inflightCounter) Load() int64 { return c.v.Load() }

// inflightLimiter 全局在途上限（规格 §10.6）：超限立即 429。
func inflightLimiter(max int64, c *inflightCounter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.Inc() > max {
				c.Dec()
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "server overloaded"})
				return
			}
			defer c.Dec()
			next.ServeHTTP(w, r)
		})
	}
}

// accessLog 请求级 Debug 追踪（规格 §7.1：生产 warn 不输出）。
func accessLog(log *logx.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reqID := r.Header.Get("X-Request-Id")
			if reqID == "" {
				reqID = uuid.NewString()
			}
			r.Header.Set("X-Request-Id", reqID)
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			if log != nil {
				log.Debug("http request",
					logx.String("request_id", reqID),
					logx.String("method", r.Method),
					logx.String("path", r.URL.Path),
					logx.Int("status", sw.status),
					logx.Duration("duration", time.Since(start)),
				)
			}
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush 委托给内层 writer（SSE 事件级冲刷必需）。
// 嵌入 http.ResponseWriter 只提升 Header/Write/WriteHeader 三个方法，
// 不带 Flush——包装层不透传的话，下游 sseWriter 拿到的 Flusher 是 nil，
// 流只能攒 4KB 缓冲批量放出，首字节延迟实测 ~145ms（Task 9 压测发现）。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 委托给内层 writer（WS 升级必需——coder/websocket Accept 要求
// http.Hijacker，补压测发现：accessLog 包裹后 resp-ws 全部升级被 501 拒）。
// 纯转发不添加状态判定：header 是否已写等语义由底层 net/http 自带（实测
// Go 1.26 写头后 Hijack 先 flush 再接管——coder/websocket Accept 正是先
// WriteHeader(101) 后 Hijack 的调用顺序，转发即兼容）。Hijack 后连接脱离
// HTTP 服务管理（http.Hijacker 文档语义）。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not implement http.Hijacker")
}

func recoverer(log *logx.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if log != nil {
						log.Error("panic recovered", logx.Any("panic", rec))
					}
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
