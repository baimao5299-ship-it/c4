package proxy

import (
	"context"
	"encoding/json"
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
// "400" = 每个非流式请求都返回 400（测 4xx 透传、不转移）。
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
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		if failMode == "500" && !stream {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
			return
		}
		if failMode == "400" && !stream {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
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
		json.NewEncoder(w).Encode(map[string]any{
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
	sched.InvalidateAllSync()

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
	ri, ok = sched.Runtime(2)
	require.True(t, ok)
	require.Equal(t, domain.Status429, ri.Status)
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
	sched.InvalidateAllSync()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 502, rec.Code, "body=%s", rec.Body.String())
	require.Empty(t, rec.Header().Get("Retry-After"))
	ri, ok := sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusErr, ri.Status)
	ri, ok = sched.Runtime(2)
	require.True(t, ok)
	require.Equal(t, domain.StatusErr, ri.Status)
}

// 4xx：确定性错误，透传上游状态码、不转移（规格 §5.3），账号不进入冷却。
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
	require.Contains(t, rec.Body.String(), "upstream rejected request")
	// 未 MarkResult：状态保持 active，不冷却
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status)
	require.Nil(t, ri.CooldownUntil)
}
