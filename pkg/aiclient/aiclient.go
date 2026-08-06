// Package aiclient 是 openai/anthropic 官方 SDK 的唯一引用点：客户端懒构建 +
// 鉴权头注入 + 非流式超时策略。协议类型（params/response/stream）直接透传为
// 调用签名——它们是协议本身，隐藏它们等于重写协议（违背"用现成库"）。
//
// 版本说明（openai-go v1.12.0 / anthropic-sdk-go v1.56.0）：客户端选项在
// option 子包（option.WithBaseURL/WithHTTPClient/WithHeader）；流式入口
// NewStreaming 在请求选项层注入 "stream": true，参数里不再有 Stream 字段，
// 流类型为 *ssestream.Stream[T]（Next()/Current()/Err() 迭代）。调用方按
// 原始请求体里的 stream 字段决定走 New 还是 NewStreaming。
package aiclient

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	anthropicstream "github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/openai/openai-go"
	openaioption "github.com/openai/openai-go/option"
	openaistream "github.com/openai/openai-go/packages/ssestream"
	"github.com/openai/openai-go/responses"

	"go-proxy-mini/internal/domain"
)

type Config struct {
	UpstreamTimeout       time.Duration // 非流式调用超时
	UpstreamStreamTimeout time.Duration // 流式 backstop（由调用方以 ctx 传入）
}

type Factory struct {
	hc  *http.Client
	cfg Config
	mu  sync.Mutex
	byT map[int64]*TemplateClients
}

type TemplateClients struct {
	chat      *openai.Client
	responses *openai.Client
	anthropic *anthropic.Client
}

func NewFactory(hc *http.Client, cfg Config) *Factory {
	return &Factory{hc: hc, cfg: cfg, byT: make(map[int64]*TemplateClients)}
}

// InvalidateAll 模板变更后丢弃所有客户端（base_url 变化生效）。
func (f *Factory) InvalidateAll() {
	f.mu.Lock()
	f.byT = make(map[int64]*TemplateClients)
	f.mu.Unlock()
}

// --- openai chat/completions ---

// ChatCompletion 非流式调用（内部注入鉴权头 + 超时）。
func (f *Factory) ChatCompletion(ctx context.Context, tpl *domain.Template, key string, params openai.ChatCompletionNewParams) (*openai.ChatCompletion, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.UpstreamTimeout)
	defer cancel()
	return f.chat(tpl).Chat.Completions.New(ctx, params, openaioption.WithHeader("Authorization", "Bearer "+key))
}

// ChatCompletionStream 流式调用；ctx 由调用方管理（含超时），本函数只注入鉴权头。
func (f *Factory) ChatCompletionStream(ctx context.Context, tpl *domain.Template, key string, params openai.ChatCompletionNewParams) *openaistream.Stream[openai.ChatCompletionChunk] {
	return f.chat(tpl).Chat.Completions.NewStreaming(ctx, params, openaioption.WithHeader("Authorization", "Bearer "+key))
}

// --- openai responses ---

func (f *Factory) Response(ctx context.Context, tpl *domain.Template, key string, params responses.ResponseNewParams) (*responses.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.UpstreamTimeout)
	defer cancel()
	return f.responses(tpl).Responses.New(ctx, params, openaioption.WithHeader("Authorization", "Bearer "+key))
}

func (f *Factory) ResponseStream(ctx context.Context, tpl *domain.Template, key string, params responses.ResponseNewParams) *openaistream.Stream[responses.ResponseStreamEventUnion] {
	return f.responses(tpl).Responses.NewStreaming(ctx, params, openaioption.WithHeader("Authorization", "Bearer "+key))
}

// --- anthropic messages ---

func (f *Factory) AnthMessage(ctx context.Context, tpl *domain.Template, key string, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.UpstreamTimeout)
	defer cancel()
	return f.anthropic(tpl).Messages.New(ctx, params, anthropicoption.WithHeader("x-api-key", key))
}

func (f *Factory) AnthMessageStream(ctx context.Context, tpl *domain.Template, key string, params anthropic.MessageNewParams) *anthropicstream.Stream[anthropic.MessageStreamEventUnion] {
	return f.anthropic(tpl).Messages.NewStreaming(ctx, params, anthropicoption.WithHeader("x-api-key", key))
}

// --- 流式原始请求（SSE relay 用） ---
// SDK 的请求层在 internal/requestconfig（不可 import），故基于共享 http.Client
// 构造原始请求：注入鉴权头、使用 SDK 客户端同款连接池与超时。
// 返回完整 *http.Response，status 检查与 body 关闭由调用方负责。

func (f *Factory) ChatCompletionStreamRaw(ctx context.Context, tpl *domain.Template, key string, body []byte) (*http.Response, error) {
	return f.rawPost(ctx, tpl, "chat/completions", "Bearer "+key, body)
}

func (f *Factory) ResponseStreamRaw(ctx context.Context, tpl *domain.Template, key string, body []byte) (*http.Response, error) {
	return f.rawPost(ctx, tpl, "responses", "Bearer "+key, body)
}

func (f *Factory) AnthMessageStreamRaw(ctx context.Context, tpl *domain.Template, key string, body []byte) (*http.Response, error) {
	return f.rawPost(ctx, tpl, "v1/messages", key, body)
}

// rawPost 构造并发出原始 POST；authHeader 为 Authorization 值（anthropic 用
// x-api-key，传 key 本身）。
func (f *Factory) rawPost(ctx context.Context, tpl *domain.Template, path, auth string, body []byte) (*http.Response, error) {
	u, err := url.Parse(tpl.BaseURL)
	if err != nil {
		return nil, err
	}
	// url.JoinPath：base 末尾带/不带 / 均可正确拼接（ResolveReference 对
	// 非尾斜杠 base 会做 RFC3986 path merge，丢掉 /v1 等前缀）。
	full := u.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if path == "v1/messages" {
		req.Header.Set("x-api-key", auth)
	} else {
		req.Header.Set("Authorization", auth)
	}
	return f.hc.Do(req)
}

// --- 客户端懒构建（每模板最多 3 个，共享 http.Client，规格 §6.1） ---

func (f *Factory) chat(tpl *domain.Template) *openai.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	tc := f.ensure(tpl.ID)
	if tc.chat == nil {
		c := openai.NewClient(
			openaioption.WithBaseURL(tpl.BaseURL),
			openaioption.WithHTTPClient(f.hc),
			// 关闭 SDK 内置重试：转移/退避由调度器统一控制（规格 §5），
			// SDK 在单次调用内静默重试会让 429 背压放大并阻塞热路径。
			openaioption.WithMaxRetries(0),
		)
		tc.chat = &c
	}
	return tc.chat
}

func (f *Factory) responses(tpl *domain.Template) *openai.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	tc := f.ensure(tpl.ID)
	if tc.responses == nil {
		c := openai.NewClient(
			openaioption.WithBaseURL(tpl.BaseURL),
			openaioption.WithHTTPClient(f.hc),
			openaioption.WithMaxRetries(0),
		)
		tc.responses = &c
	}
	return tc.responses
}

func (f *Factory) anthropic(tpl *domain.Template) *anthropic.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	tc := f.ensure(tpl.ID)
	if tc.anthropic == nil {
		c := anthropic.NewClient(
			anthropicoption.WithBaseURL(tpl.BaseURL),
			anthropicoption.WithHTTPClient(f.hc),
			anthropicoption.WithMaxRetries(0),
		)
		tc.anthropic = &c
	}
	return tc.anthropic
}

func (f *Factory) ensure(id int64) *TemplateClients {
	tc, ok := f.byT[id]
	if !ok {
		tc = &TemplateClients{}
		f.byT[id] = tc
	}
	return tc
}
