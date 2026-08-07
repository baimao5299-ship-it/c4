package proxy

import (
	"net/http"

	"go-proxy-mini/internal/domain"
)

// HandleAnthropic 转发 /v1/messages（anthropic 格式）。全部逻辑在
// 通用骨架 handleFormat + anthropicCaller（Phase 2 转发骨架通用化），本方法
// 只是端点入口委托。测试按处理函数直接调用（forward_ext_test.go 等），故入口保留。
func (p *Proxy) HandleAnthropic(w http.ResponseWriter, r *http.Request) {
	p.handleFormat(domain.FormatAnthropic, w, r)
}
