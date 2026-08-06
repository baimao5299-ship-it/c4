// Package server 装配 chi 路由：/admin/*（admin token）+ 三个 AI 端点 + /healthz。
package server

import (
	"io/fs"
	"net/http"
	"runtime"
	"strings"
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
	WebFS             fs.FS        // 前端构建产物（nil = 不挂静态资源）
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

	// 静态资源 + SPA fallback：必须在 admin/AI/healthz 之后注册。
	// 说明：chi 的 NotFound() 会向已 Mount 的子路由传播（updateSubRoutes），
	// 因此这里在 Mount("/", AIHandler) 之后设置 NotFound，AI 路由的未匹配
	// 路径会进入同一 fallback；/admin/* 经 Handle 注册不受影响。
	if opts.WebFS != nil {
		r.Handle("/assets/*", http.FileServerFS(opts.WebFS))
		// favicon 位于 dist 根（index.html 引用 /favicon.svg），不走 SPA fallback，
		// 否则会返回 index.html（content-type text/html，浏览器拒绝渲染）。
		r.Handle("/favicon.svg", http.FileServerFS(opts.WebFS))
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			index, err := fs.ReadFile(opts.WebFS, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
		})
		// SPA fallback：非 API/静态路径回 index.html
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/admin") || strings.HasPrefix(r.URL.Path, "/v1") || r.URL.Path == "/healthz" {
				http.NotFound(w, r)
				return
			}
			index, err := fs.ReadFile(opts.WebFS, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
		})
	}

	s.handler = r
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }
