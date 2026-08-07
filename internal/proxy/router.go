package proxy

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"go-proxy-mini/internal/domain"
)

// AIRouter 挂载三个 AI 端点（规格 §6.1/§9）：路径决定请求格式，
// 全部走通用转发骨架 handleFormat（Phase 2：UpstreamCaller 注册表分发）。
func AIRouter(p *Proxy) http.Handler {
	r := chi.NewRouter()
	r.Post("/v1/chat/completions", func(w http.ResponseWriter, req *http.Request) {
		p.handleFormat(domain.FormatOpenAIChat, w, req)
	})
	r.Post("/v1/responses", func(w http.ResponseWriter, req *http.Request) {
		p.handleFormat(domain.FormatOpenAIResponses, w, req)
	})
	r.Post("/v1/messages", func(w http.ResponseWriter, req *http.Request) {
		p.handleFormat(domain.FormatAnthropic, w, req)
	})
	return r
}
