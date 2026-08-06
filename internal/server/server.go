// Package server 装配 chi 路由：/admin/*（admin token）+ 三个 AI 端点 + /healthz。
package server

import (
	"net/http"
	"runtime"
	"time"

	"github.com/go-chi/chi/v5"

	"go-proxy-mini/pkg/logx"
)

type Options struct {
	AdminToken        string
	MaxInflight       int64
	ReadHeaderTimeout time.Duration
	MaxHeaderBytes    int
	AdminHandler      http.Handler // 已挂 /admin/* 路由
	AIHandler         http.Handler // proxy 三个端点
	Logger            *logx.Logger
}

type Server struct {
	opts     Options
	inflight inflightCounter
	handler  http.Handler
}

func NewServer(opts Options) *Server {
	if opts.ReadHeaderTimeout == 0 {
		opts.ReadHeaderTimeout = 10 * time.Second
	}
	if opts.MaxHeaderBytes == 0 {
		opts.MaxHeaderBytes = 1 << 20
	}
	if opts.MaxInflight == 0 {
		opts.MaxInflight = 50000
	}
	s := &Server{opts: opts}

	r := chi.NewRouter()
	r.Use(recoverer(opts.Logger))
	r.Use(accessLog(opts.Logger))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		writeJSON(w, http.StatusOK, map[string]any{
			"inflight":   s.inflight.Load(),
			"goroutines": runtime.NumGoroutine(),
			"heap":       ms.HeapAlloc,
		})
	})

	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler { // admin token 认证
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("Authorization") != "Bearer "+opts.AdminToken {
					writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
					return
				}
				next.ServeHTTP(w, req)
			})
		})
		if opts.AdminHandler != nil {
			// 用 Handle 而非 Mount：chi v5.3.1 对同一 pattern 重复 Mount 会 panic，
			// 且 AI 组已挂 Mount("/", ...)。Handle 不剥离前缀，AdminHandler
			// （HandlerWithOptions BaseURL="/admin"）按完整路径 /admin/* 匹配。
			r.Handle("/admin/*", opts.AdminHandler)
		}
	})

	r.Group(func(r chi.Router) {
		r.Use(inflightLimiter(opts.MaxInflight, &s.inflight))
		if opts.AIHandler != nil {
			r.Mount("/", opts.AIHandler)
		}
	})
	s.handler = r
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }
