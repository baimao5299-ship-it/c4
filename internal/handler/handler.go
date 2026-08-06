// Package handler 实现 /admin/* 的 HTTP 处理：OpenAPI 契约层（oapi-codegen
// 生成的 ServerInterface + chi 路由），JSON in/out。
package handler

import (
	"encoding/json"
	"net/http"

	"go-proxy-mini/internal/service"
)

// AdminAPI 实现生成的 ServerInterface（契约层唯一实现）。
// 注：生成代码自带包级 func Handler(...) 路由助手，故实现结构体命名
// AdminAPI 以避免与生成的 Handler 函数重名。
type AdminAPI struct {
	svc *service.Service
}

// New 构造契约处理器（路由由 HandlerWithOptions 生成）。
func New(svc *service.Service) *AdminAPI {
	return &AdminAPI{svc: svc}
}

// Router 返回带 /admin 前缀的 chi 路由（替代原 Routes/RoutesMux）。
func (h *AdminAPI) Router() http.Handler {
	return HandlerWithOptions(h, ChiServerOptions{BaseURL: "/admin"})
}

// RoutesMux 兼容保留：cmd/server/main.go 仍以 Handle("/admin/*") 挂载，
// 后续任务改接 Router 后可删除。
func (h *AdminAPI) RoutesMux() http.Handler {
	return h.Router()
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

// deref 返回指针指向的值；nil 时返回零值。
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
