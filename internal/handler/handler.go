// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package handler 实现 /admin/* 的 HTTP 处理：OpenAPI 契约层（oapi-codegen
// 生成的 ServerInterface + chi 路由），JSON in/out。
package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/is7qin/c3api/internal/service"
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
// ErrorHandlerFunc 覆盖生成的默认 http.Error 纯文本 400：参数绑定失败
// （InvalidParamFormatError 等）统一输出契约 ErrorResponse（{"error": ...}）。
func (h *AdminAPI) Router() http.Handler {
	return HandlerWithOptions(h, ChiServerOptions{
		BaseURL: "/admin",
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, http.StatusBadRequest, err.Error())
		},
	})
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

// ptr 返回指向 v 的指针（响应契约字段赋值用）。
func ptr[T any](v T) *T { return &v }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// normalizeIDs 校验批量 ids 1–100 条且去重（返回去重后列表，条数按去重后计）。
func normalizeIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, errors.New("ids must contain 1-100 entries")
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// writeBatchServiceErr 批量操作的错误映射：404 需携带缺失 id 信息（service
// 层把 repo 的 "id=%d missing" 包装进 ErrNotFound），其余走 writeServiceErr。
func writeBatchServiceErr(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeServiceErr(w, err)
}
