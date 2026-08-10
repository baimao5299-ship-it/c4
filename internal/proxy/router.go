package proxy

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"go-proxy-mini/internal/domain"
)

// AIRouter 挂载 AI 端点（规格 §6.1/§9）：路径决定请求格式，全部走通用转发
// 骨架 handleFormat（Phase 2：UpstreamCaller 注册表分发）；resp-ws 与 HTTP
// responses 同路径（真实客户端无 /ws 后缀）——/v1/responses 带 upgrade 头
// → 按 resp-ws 处理，走专用编排 HandleResponsesWS（caller_responses_ws.go）。
func AIRouter(p *Proxy) http.Handler {
	r := chi.NewRouter()
	r.Post("/v1/chat/completions", func(w http.ResponseWriter, req *http.Request) {
		p.handleFormat(domain.FormatOpenAIChat, w, req)
	})
	r.Post("/v1/responses", func(w http.ResponseWriter, req *http.Request) {
		if isWebSocketUpgrade(req) {
			p.HandleResponsesWS(w, req)
			return
		}
		p.handleFormat(domain.FormatOpenAIResponses, w, req)
	})
	// WS 升级请求是 GET（RFC 6455），HTTP responses 是 POST——GET 路由只放行
	// upgrade，普通 GET 保持 405（API 语义不变）。
	r.Get("/v1/responses", func(w http.ResponseWriter, req *http.Request) {
		if !isWebSocketUpgrade(req) {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
			return
		}
		p.HandleResponsesWS(w, req)
	})
	r.Post("/v1/messages", func(w http.ResponseWriter, req *http.Request) {
		p.handleFormat(domain.FormatAnthropic, w, req)
	})
	return r
}
