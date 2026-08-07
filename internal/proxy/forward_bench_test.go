package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-proxy-mini/internal/credential"
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/rule"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/usage"
	"go-proxy-mini/pkg/aiclient"
	"go-proxy-mini/pkg/cryptox"
)

// 转发热路径基准（Task 3）：完整 Proxy + fake 上游 + 固定 body，流式/非流式
// 各一。经 AIRouter 走生产分发路径（main 上 = HandleXxx，分支上 = handleFormat），
// 与 main 基线对比 alloc/op 零增长是硬标准。
// 基准文件随分支提交；main 基线用临时 worktree 跑同一文件（比 stash 安全）。

// benchUpstream 极简 chat 上游（与 proxy_test.fakeOpenAI 同构：stream 按 body 分支）。
func benchUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
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
			"id": "c1", "object": "chat.completion", "model": body["model"],
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
		})
	}))
}

// benchProxy 构造完整 Proxy（与 newTestProxyTplTimeoutLogs 同构；bench 内无 *testing.T）。
func benchProxy(upstream string) *Proxy {
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
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
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	if err := re.Reload(context.Background()); err != nil {
		panic(err)
	}
	sched := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil)
	if err := sched.InvalidateAllSync(); err != nil {
		panic(err)
	}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		LogRetentionDays: 30, StatsFlushInterval: time.Hour,
	}, noopLogStore{}, noopStatStore{}, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]int64{cryptox.HashKey("gk-1"): 10}}, nil)
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second,
	})
	return New(cfg, sched, credential.New(), rec, clients, auth, nil)
}

func benchForwardChat(b *testing.B, streaming bool) {
	up := benchUpstream()
	defer up.Close()
	p := benchProxy(up.URL + "/v1")
	r := AIRouter(p)
	stream := "false"
	if streaming {
		stream = "true"
	}
	payload := []byte(`{"model":"gpt-4o","stream":` + stream + `,"messages":[{"role":"user","content":"hi"}]}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer gk-1")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
}

func BenchmarkForwardChatNonStreaming(b *testing.B) { benchForwardChat(b, false) }
func BenchmarkForwardChatStreaming(b *testing.B)    { benchForwardChat(b, true) }
