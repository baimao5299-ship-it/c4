package proxy

import (
	"net/http"

	"go-proxy-mini/internal/domain"
)

// HandleChat 转发 /v1/chat/completions（openai-chat 格式）。全部逻辑在
// 通用骨架 handleFormat + chatCaller（Phase 2 转发骨架通用化），本方法
// 只是端点入口委托。测试按处理函数直接调用（proxy_test.go 等），故入口保留。
func (p *Proxy) HandleChat(w http.ResponseWriter, r *http.Request) {
	p.handleFormat(domain.FormatOpenAIChat, w, r)
}
