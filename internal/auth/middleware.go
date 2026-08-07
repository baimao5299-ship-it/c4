package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"go-proxy-mini/internal/domain"
)

// UserStatusProvider 用户状态快照（proxy.Auth 实现；用户变更走 invalidate →
// Auth.Reload 全量刷新，RequireJWT 不用 DB 直查——评审定夺②）。
type UserStatusProvider interface {
	UserStatus(userID int64) (domain.UserStatus, bool)
}

type ctxClaimsKey struct{}

// ClaimsFrom 从请求 context 取已验证 claims（RequireJWT 之后可用）。
func ClaimsFrom(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxClaimsKey{}).(*Claims)
	return c, ok
}

// RequireJWT /user 组中间件：验证 Bearer JWT + 内存快照用户状态校验。
// 用户禁用 → 立即拒绝（快照刷新走 invalidate）；JWT 15min 短时效兜底。
func RequireJWT(iss *Issuer, users UserStatusProvider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || raw == "" {
				writeUnauthorized(w)
				return
			}
			claims, err := iss.Verify(raw)
			if err != nil {
				writeUnauthorized(w)
				return
			}
			// 快照用户状态校验：禁用即拒（无需等 JWT 过期）
			if st, ok := users.UserStatus(claims.UserID); ok && st != domain.UserStatusActive {
				writeUnauthorized(w)
				return
			}
			ctx := context.WithValue(r.Context(), ctxClaimsKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole 端点级 RBAC 声明：claims.Role 必须 ∈ 允许角色。当前两级角色下
// 用户面端点不区分角色（platform_admin 也可用 /user 面），本中间件供后续
// 细分权限使用（/admin 组鉴权 = 静态 token OR platform_admin JWT）。
func RequireRole(roles ...domain.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFrom(r.Context())
			if !ok {
				writeJSONStatus(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			for _, role := range roles {
				if claims.Role == string(role) {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeJSONStatus(w, http.StatusForbidden, "forbidden")
		})
	}
}

func writeUnauthorized(w http.ResponseWriter) {
	writeJSONStatus(w, http.StatusUnauthorized, "unauthorized")
}

func writeJSONStatus(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + strconv.Quote(msg) + `}`))
}
