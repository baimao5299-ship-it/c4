// Package handler 实现 /admin/* 的 HTTP 处理（JSON in/out，chi 路由）。
package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"go-proxy-mini/internal/service"
)

type Handler struct {
	svc *service.Service
}

func New(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// Routes 挂载全部 admin 路由（不含认证中间件，由 server 层加）。
func (h *Handler) Routes(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		r.Route("/templates", func(r chi.Router) {
			r.Post("/", h.createTemplate)
			r.Get("/", h.listTemplates)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", h.getTemplate)
				r.Put("/", h.updateTemplate)
				r.Delete("/", h.deleteTemplate)
			})
		})
		r.Route("/accounts", func(r chi.Router) {
			r.Post("/", h.createAccount)
			r.Get("/", h.listAccounts)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", h.getAccount)
				r.Put("/", h.updateAccount)
				r.Delete("/", h.deleteAccount)
			})
		})
		r.Route("/groups", func(r chi.Router) {
			r.Post("/", h.createGroup)
			r.Get("/", h.listGroups)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", h.getGroup)
				r.Put("/", h.updateGroup)
				r.Delete("/", h.deleteGroup)
				r.Put("/accounts", h.setGroupAccounts)
				r.Post("/rotate-key", h.rotateGroupKey)
			})
		})
		r.Get("/logs", h.queryLogs)
		r.Get("/stats", h.queryStats)
	})
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
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
