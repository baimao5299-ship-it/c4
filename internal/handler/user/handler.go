// Package user 实现 /user 面 HTTP 处理（OpenAPI tag: user 的独立
// ServerInterface）：认证（register/login 公开；me 及业务端点 JWT 保护）+
// 业务端点（groups/keys/logs/stats，Phase 3a Task 4）。
package user

import (
	"encoding/json"
	"errors"
	"net/http"

	"go-proxy-mini/internal/service"
)

// UserAPI 实现生成的 ServerInterface（user 面唯一实现）。
type UserAPI struct {
	svc *service.Service
	iss tokenIssuer
}

// tokenIssuer JWT 签发（*auth.Issuer 实现；测试可注入替身）。
type tokenIssuer interface {
	Issue(userID int64, email, role string) (string, error)
}

// New 构造契约处理器（路由由 Router 组装）。
func New(svc *service.Service, iss tokenIssuer) *UserAPI {
	return &UserAPI{svc: svc, iss: iss}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// writeServiceErr service 错误映射（与 admin 面同语义）。
func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrInvalidCredentials):
		writeErr(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, service.ErrSignupDisabled):
		writeErr(w, http.StatusForbidden, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}
