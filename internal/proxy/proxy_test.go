package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/usage"
	"go-proxy-mini/pkg/aiclient"
	"go-proxy-mini/pkg/cryptox"
)

// --- 假上游：SSE 流式 chat/completions ---
// failMode: "" = 正常；"429" = 每个非流式请求都返回 429（测 failover）；
// "500" = 每个非流式请求都返回 500（测 ResultError→502）；
// "400" = 每个非流式请求都返回 400（测 4xx 透传、不转移）；
// "abort-stream" = 流式响应中途发非法事件（解码失败 → 流 Err() 非空，测中止路径）。
func fakeOpenAI(t *testing.T, failMode string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
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
			w.Header().Set("x-ratelimit-reset-requests", "5s")
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		if failMode == "500" && !stream {
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
			return
		}
		if failMode == "400" && !stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if failMode == "abort-stream" && stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}],"usage":null}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, "data: {oops\n\n") // 非法事件：SDK 解码失败 → 流中止
			fl.Flush()
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			chunks := [2]string{
				`{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}],"usage":null}`,
				`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
				fl.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion",
			"model": body["model"],
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
		})
	}))
	return srv
}

type noopKeyLoader struct{ keys map[string]int64 }

func (n noopKeyLoader) LoadGroupKeys(ctx context.Context) (map[string]int64, error) {
	return n.keys, nil
}

type noopLogStore struct{}

func (noopLogStore) InsertBatch(ctx context.Context, l []*domain.UsageLog) error { return nil }
func (noopLogStore) PurgeLogs(ctx context.Context, t time.Time) error            { return nil }

type noopStatStore struct{}

func (noopStatStore) Upsert(ctx context.Context, b []*domain.StatBucket) error { return nil }

type noopLoader struct{ accs map[int64][]*domain.Account }

func (n noopLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	return n.accs, nil
}
func (n noopLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	return n.accs[id], nil
}
func (n noopLoader) UpdateAccountStatus(ctx context.Context, id int64, s domain.AccountStatus, c *time.Time, e *string) error {
	return nil
}

func newTestProxy(t *testing.T, upstream string, accountID int64) *Proxy {
	t.Helper()
	return newTestProxyCapture(t, upstream, accountID, true)
}

func newTestProxyCapture(t *testing.T, upstream string, accountID int64, usageCapture bool) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		DefaultFormat: domain.FormatOpenAIChat, Models: []string{"gpt-4o"},
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: accountID, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
	}}}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM:           0, UsageCapture: usageCapture,
	}
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, Cooldown429: 30 * time.Second,
		BackoffBase: 5 * time.Second, BackoffMax: time.Minute, SyncInterval: time.Hour,
	}, noopLoader{accs: accs}, nil)
	require.NoError(t, sched.InvalidateAllSync())
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		LogRetentionDays: 30, StatsFlushInterval: time.Hour,
	}, noopLogStore{}, noopStatStore{}, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]int64{cryptox.HashKey("gk-1"): 10}}, nil)
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	return New(cfg, sched, rec, clients, auth, nil)
}

func TestProxyStreamingChat(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, "data: [DONE]")
	require.Contains(t, body, `"content":"hi"`)
	require.Contains(t, body, `"prompt_tokens":5`, "usage captured from final chunk")
	// 成功路径必须释放并发槽并记录用量
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")
}

// SSE 事件级冲刷回归（Task 9 压测发现）：sseWriter 必须每事件调用 http.Flusher.Flush()。
// 只刷 bufio 不刷 Flusher 时，http.Server 内部 4KB 缓冲攒批放出，流式首字节
// 延迟实测 145ms（修复后 ~1ms，见 docs/superpowers/plans/loadtest-results.md）。
// ResponseRecorder 实现 Flusher：首个事件写出后 Flushed 必须为真。
func TestProxyStreamingSSEFlushesPerEvent(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.True(t, rec.Flushed, "SSE 每事件必须冲刷（http.Flusher），首个事件后 Flushed 为真")
	require.Contains(t, rec.Body.String(), "data: [DONE]")
}

func TestProxyAuthRejected(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 401, rec.Code)
}

func TestProxyFailoverOn429(t *testing.T) {
	// 两个账号指向同一个会 429 的上游：第一个失败后转移第二个（同样失败则最终 429）
	up := fakeOpenAI(t, "429")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)
	// 第二个账号
	tpl2 := &domain.Template{ID: 2, Name: "t2", BaseURL: up.URL + "/v1", DefaultFormat: domain.FormatOpenAIChat, Models: []string{"gpt-4o"}}
	sched := p.sched
	acc2 := &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
	loader := p.sched.Loader().(noopLoader)
	loader.accs[10] = append(loader.accs[10], acc2)
	require.NoError(t, sched.InvalidateAllSync())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 429, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "1", rec.Header().Get("Retry-After"), "429 最终失败回 Retry-After")
	// 两个账号都进入 429 冷却：Runtime 视图可查
	ri, ok := sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.Status429, ri.Status)
	require.Zero(t, ri.Concurrency, "failover 后并发槽必须全部释放")
	ri, ok = sched.Runtime(2)
	require.True(t, ok)
	require.Equal(t, domain.Status429, ri.Status)
	require.Zero(t, ri.Concurrency, "failover 后并发槽必须全部释放")
	// 耗尽路径（请求已完成）：以最后一次尝试的结果记一条用量
	require.Equal(t, 1, p.rec.Pending(), "failover 耗尽必须记一条用量")
}

// 5xx：触发 failover 与 MarkResult(ResultError)；全部尝试失败最终回 502（非 429 不设 Retry-After）。
func TestProxyFailoverOn5xx(t *testing.T) {
	up := fakeOpenAI(t, "500")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)
	tpl2 := &domain.Template{ID: 2, Name: "t2", BaseURL: up.URL + "/v1", DefaultFormat: domain.FormatOpenAIChat, Models: []string{"gpt-4o"}}
	sched := p.sched
	acc2 := &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
	loader := p.sched.Loader().(noopLoader)
	loader.accs[10] = append(loader.accs[10], acc2)
	require.NoError(t, sched.InvalidateAllSync())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 502, rec.Code, "body=%s", rec.Body.String())
	require.Empty(t, rec.Header().Get("Retry-After"))
	ri, ok := sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status)
	require.Zero(t, ri.Concurrency, "failover 后并发槽必须全部释放")
	ri, ok = sched.Runtime(2)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status)
	require.Zero(t, ri.Concurrency, "failover 后并发槽必须全部释放")
	// 耗尽路径（请求已完成）：以最后一次尝试的结果记一条用量
	require.Equal(t, 1, p.rec.Pending(), "failover 耗尽必须记一条用量")
}

// 回归（评审 Critical）：failover 耗尽时尾部 Select 为"不存在的下一次尝试"预取
// 并发槽——若选中第三个健康账号则槽位永不释放（CAS 抢占、仅 Release 递减、无回收）。
// 3 账号全健康 + FailoverAttempts=2：前两轮 429 后，泄漏版本会在循环退出前
// 抢走剩余健康账号的槽（Concurrency==1），修复版本不预选（全部为 0）。
func TestProxyFailoverExhaustedNoLeak(t *testing.T) {
	up := fakeOpenAI(t, "429")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)
	sched := p.sched
	loader := p.sched.Loader().(noopLoader)
	for i := int64(2); i <= 3; i++ {
		tpl := &domain.Template{ID: i, Name: fmt.Sprintf("t%d", i), BaseURL: up.URL + "/v1",
			DefaultFormat: domain.FormatOpenAIChat, Models: []string{"gpt-4o"}}
		loader.accs[10] = append(loader.accs[10], &domain.Account{
			ID: i, TemplateID: i, Template: tpl, UpstreamKey: "sk-upstream",
			Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
		})
	}
	require.NoError(t, sched.InvalidateAllSync())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 429, rec.Code, "body=%s", rec.Body.String())
	for id := int64(1); id <= 3; id++ {
		ri, ok := sched.Runtime(id)
		require.True(t, ok)
		require.Zero(t, ri.Concurrency, "account %d 并发槽必须全部释放", id)
	}
	require.Equal(t, 1, p.rec.Pending(), "耗尽路径必须记一条用量")
}

// 4xx：确定性错误，透传上游状态码与原始 body、不转移（规格 §5.3），账号不进入冷却。
func TestProxyPassthrough4xx(t *testing.T) {
	up := fakeOpenAI(t, "400")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 400, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"bad request"`, "4xx 必须透传上游原始 body")
	require.NotContains(t, rec.Body.String(), "upstream rejected request", "透传 body 时不得回退网关文案")
	// 未 MarkResult：状态保持 active，不冷却
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status)
	require.Nil(t, ri.CooldownUntil)
	require.Zero(t, ri.Concurrency, "4xx 透传也必须释放并发槽")
	// 请求已完成（上游消费了请求）：必须记录用量
	require.Equal(t, 1, p.rec.Pending(), "4xx 透传必须记录用量")
}

// 流式中止：上游在流中途发非法事件（解码失败）→ ResultError + 释放并发槽 + ErrAbort 记录。
func TestProxyStreamAbortFreesSlot(t *testing.T) {
	up := fakeOpenAI(t, "abort-stream")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "中止记 ResultError")
	require.Zero(t, ri.Concurrency, "中止路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "中止路径记 ErrAbort 用量")
}

// failingResponseWriter 模拟客户端断开：所有写出都失败。
type failingResponseWriter struct{}

func (failingResponseWriter) Header() http.Header         { return http.Header{} }
func (failingResponseWriter) Write(p []byte) (int, error) { return 0, errors.New("client gone") }
func (failingResponseWriter) WriteHeader(int)             {}

// 客户端断开：SSE 写出失败 → ResultError + 释放并发槽（无法判定结果，不记用量）。
func TestProxyClientDisconnectFreesSlot(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	p.HandleChat(failingResponseWriter{}, req)

	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "客户端断开记 ResultError")
	require.Zero(t, ri.Concurrency, "客户端断开必须释放并发槽")
	require.Zero(t, p.rec.Pending(), "客户端断开不记用量")
}

// UsageCapture=false：Record 不得被调用（channel 零填充，否则饱和后阻塞热路径）。
func TestProxyUsageCaptureDisabled(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxyCapture(t, up.URL+"/v1", 1, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Zero(t, p.rec.Pending(), "UsageCapture=false 时不得入队")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "并发槽仍须释放")
}

// 回归：单账号 429 冷却后，失败转移中途 Select 失败（nil, ErrNoAvailable）时
// 耗尽路径不得解引用 nil Selection（此前 panic → 500；应 429）。
func TestProxyChatFailoverSingleAccountNoPanic(t *testing.T) {
	up := fakeOpenAI(t, "429")
	defer up.Close()
	p := newTestProxy(t, up.URL+"/v1", 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	require.NotPanics(t, func() { p.HandleChat(rec, req) })
	require.Equal(t, 429, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "1", rec.Header().Get("Retry-After"))
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "并发槽必须释放")
	require.Equal(t, 1, p.rec.Pending(), "耗尽路径必须记一条用量")
}
