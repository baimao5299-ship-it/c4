package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicstream "github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/usage"
	"go-proxy-mini/pkg/aiclient"
	"go-proxy-mini/pkg/cryptox"
)

// --- 假上游：openai /v1/responses（Responses API） ---
// failMode: "" = 正常；"429" = 非流式 429（测 failover）；"400" = 非流式 400（测透传）。
func fakeResponses(t *testing.T, failMode string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if failMode == "429" && !stream {
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		if failMode == "400" && !stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "rsp_1", "object": "response", "created_at": 1750000000,
			"status": "completed", "model": body["model"], "output": []any{},
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 5, "total_tokens": 8},
		})
	}))
	return srv
}

// --- 假上游：anthropic /v1/messages ---
func fakeAnthropic(t *testing.T, failMode string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("x-api-key") != "sk-upstream" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if failMode == "429" && !stream {
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		if failMode == "400" && !stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			// anthropic SSE 带 event: 行；SDK 按 event 类型分发事件（无 event 行的事件被跳过）。
			// 用量按真实 API 分布：input_tokens 在 message_start.message.usage，
			// output_tokens 在 message_delta.usage（message_delta 不带 input_tokens）。
			fmt.Fprint(w, `event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"gpt-4o","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":0}}}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, `event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, `event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, `event: message_stop`+"\n"+`data: {"type":"message_stop"}`+"\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant",
			"model": body["model"], "content": []any{map[string]any{"type": "text", "text": "hi"}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 5},
		})
	}))
	return srv
}

// captureLogStore 捕获落库的用量明细（用量值断言用）。
type captureLogStore struct {
	mu   sync.Mutex
	logs []*domain.UsageLog
}

func (c *captureLogStore) InsertBatch(ctx context.Context, l []*domain.UsageLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, l...)
	return nil
}

func (c *captureLogStore) PurgeLogs(ctx context.Context, t time.Time) error { return nil }

// newTestProxyFormat 构造指定模板默认格式的测试代理（调度器按模板 FormatFor 做格式硬过滤）。
func newTestProxyFormat(t *testing.T, upstream string, format domain.RequestFormat) *Proxy {
	t.Helper()
	return newTestProxyFormatLogs(t, upstream, format, noopLogStore{})
}

// newTestProxyFormatLogs 同 newTestProxyFormat，但允许注入 LogInserter（用量断言用捕获实现）。
func newTestProxyFormatLogs(t *testing.T, upstream string, format domain.RequestFormat, logs usage.LogInserter) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		DefaultFormat: format, Models: []string{"gpt-4o"},
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
	}}}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM:           0, UsageCapture: true,
	}
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, Cooldown429: 30 * time.Second,
		BackoffBase: 5 * time.Second, BackoffMax: time.Minute, SyncInterval: time.Hour,
	}, noopLoader{accs: accs}, nil)
	require.NoError(t, sched.InvalidateAllSync())
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		LogRetentionDays: 30, StatsFlushInterval: time.Hour,
	}, logs, noopStatStore{}, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]int64{cryptox.HashKey("gk-1"): 10}}, nil)
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	return New(cfg, sched, rec, clients, auth, nil)
}

func TestProxyResponsesNonStreaming(t *testing.T) {
	up := fakeResponses(t, "")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL+"/v1", domain.FormatOpenAIResponses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "rsp_1", resp["id"])
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")
}

func TestProxyResponsesStreaming(t *testing.T) {
	up := fakeResponses(t, "")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL+"/v1", domain.FormatOpenAIResponses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, `"type":"response.output_text.delta"`)
	require.Contains(t, body, `"input_tokens":3`, "usage captured from response.completed event")
	require.Contains(t, body, "data: [DONE]")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")
}

func TestProxyAnthropicNonStreaming(t *testing.T) {
	up := fakeAnthropic(t, "")
	defer up.Close()
	// anthropic SDK 的路径自带 v1 前缀（v1/messages），base 不能含 /v1
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "msg_1", resp["id"])
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")
}

func TestProxyResponsesFailoverExhausted429(t *testing.T) {
	up := fakeResponses(t, "429")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL+"/v1", domain.FormatOpenAIResponses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "1", rec.Header().Get("Retry-After"))
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "失败转移耗尽后并发槽必须释放")
	require.Equal(t, 1, p.rec.Pending(), "耗尽路径必须记录一条用量")
}

func TestProxyAnthropicPassthrough4xx(t *testing.T) {
	up := fakeAnthropic(t, "400")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "upstream rejected request")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "4xx 透传后并发槽必须释放")
	require.Equal(t, 1, p.rec.Pending(), "4xx 路径必须记录一条用量")
}

func TestProxyAnthropicStreaming(t *testing.T) {
	up := fakeAnthropic(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatAnthropic, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, `"type":"content_block_delta"`)
	require.Contains(t, body, `"input_tokens":3`, "input_tokens passthrough from message_start event")
	require.Contains(t, body, `"output_tokens":5`, "output_tokens passthrough from message_delta event")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")

	// 用量值断言（评审发现修复）：prompt_tokens 来自 message_start.message.usage，
	// completion_tokens 来自 message_delta.usage，total 为两者之和——此前只累计
	// message_delta 导致 prompt_tokens 恒为 0。
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one usage log")
	lg := store.logs[0]
	require.Equal(t, int64(3), lg.PromptTokens, "input_tokens from message_start.message.usage")
	require.Equal(t, int64(5), lg.CompletionTokens, "output_tokens from message_delta.usage")
	require.Equal(t, int64(8), lg.TotalTokens, "total = input + output")
}

// parseAnthropicSSE 按 anthropic 协议解析输出：每个 data 块前必须有 event: 行，
// 且 event 类型与 data JSON 的 "type" 字段一致（data-only 事件官方 SDK 静默跳过）。
func parseAnthropicSSE(t *testing.T, body string) []string {
	t.Helper()
	var (
		types []string
		cur   string
	)
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			cur = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" { // 代理收尾标记，非 anthropic 协议事件
				continue
			}
			var ev struct {
				Type string `json:"type"`
			}
			require.NoError(t, json.Unmarshal([]byte(payload), &ev))
			require.Equal(t, ev.Type, cur, "event: 行与 JSON type 不一致: %q", line)
			types = append(types, ev.Type)
			cur = ""
		}
	}
	return types
}

// 回归（评审 Important）：/v1/messages 流式输出必须带 event: <type> 行——
// anthropic 官方 SDK 按 event: 行类型分发，data-only 事件被静默跳过 → 官方客户端拿到空流。
func TestProxyAnthropicStreamingSSEFraming(t *testing.T) {
	up := fakeAnthropic(t, "")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	// 1) 文本级解析：event: 行存在且与 JSON type 一致
	want := []string{"message_start", "content_block_delta", "message_delta", "message_stop"}
	require.Equal(t, want, parseAnthropicSSE(t, rec.Body.String()))

	// 2) 官方 SDK 客户端消费代理输出：event: 行缺失时流为空（修复前即空流）
	stream := anthropicstream.NewStream[anthropic.MessageStreamEventUnion](
		anthropicstream.NewDecoder(&http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(rec.Body.String())),
			Request:    httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		}), nil)
	var got []string
	for stream.Next() {
		got = append(got, stream.Current().Type)
	}
	require.NoError(t, stream.Err())
	require.Equal(t, want, got)
}
