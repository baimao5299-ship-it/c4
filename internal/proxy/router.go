package proxy

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// AIRouter 挂载三个 AI 端点（规格 §6.1/§9）：路径决定请求格式。
func AIRouter(p *Proxy) http.Handler {
	r := chi.NewRouter()
	r.Post("/v1/chat/completions", p.HandleChat)
	r.Post("/v1/responses", p.HandleResponses)
	r.Post("/v1/messages", p.HandleAnthropic)
	return r
}
