// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChatCompletionStreamRawHeadersAndPath(t *testing.T) {
	var gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	// 裸根约定：base_url 不含 /v1，openai 系由 rawPost 补 /v1 后拼路径
	// （/v1/chat/completions）；若 base 带 /v1 会拼出 /v1/v1/... 404。
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL, "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "Bearer sk-test", gotAuth)
	require.Equal(t, "application/json", gotCT)
}

func TestAnthMessageStreamRawUsesXAPIKeyAndV1Path(t *testing.T) {
	var gotKey, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotCT = r.Header.Get("Content-Type")
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {}\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.AnthMessageStreamRaw(context.Background(), 1, srv.URL, "sk-anth", []byte(`{"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "sk-anth", gotKey)
	require.Equal(t, "application/json", gotCT)
}

func TestResponseStreamRawPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ResponseStreamRaw(context.Background(), 1, srv.URL, "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestStreamRawPreservesRequestBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL, "sk-test", []byte(`{"model":"gpt-4o","stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "gpt-4o", gotBody["model"])
	require.Equal(t, true, gotBody["stream"])
}

func TestStreamRawNon200ResponseReturnsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL, "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestStreamRawBaseURLWithTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	// 尾斜杠：裸根 + "/" 同样被 openaiBaseURL 归一（TrimSuffix）后补 /v1
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL+"/", "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestStreamRawBaseURLChangeConverges 评审 C1 回归：URL 缓存键含 base_url
// 快照——同模板 ID 直接改 base_url（绕过管理 API 的 DB 直改 + 周期同步下发新
// 快照）后，新流量必须立即打到新地址；旧实现键仅 templateID，缓存不失效 →
// 流量打旧上游。模拟：同一 Factory、同模板 ID，先后传两个不同 base_url。
func TestStreamRawBaseURLChangeConverges(t *testing.T) {
	var hit atomic.Value // string：记录请求打到哪个上游
	serve := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit.Store(name)
			w.WriteHeader(200)
			_, _ = w.Write([]byte("data: x\n\n"))
		}))
	}
	oldSrv := serve("old")
	defer oldSrv.Close()
	newSrv := serve("new")
	defer newSrv.Close()

	f := NewFactory(oldSrv.Client(), Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, oldSrv.URL, "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "old", hit.Load(), "首访打旧上游")

	// 同模板 ID、新 base_url（DB 直改后周期同步下发的新快照）→ 必须收敛到新地址
	resp, err = f.ChatCompletionStreamRaw(context.Background(), 1, newSrv.URL, "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "new", hit.Load(), "缓存键含 baseURL：新快照立即收敛，不得打旧上游")

	// 旧快照键仍可复用（无重建）——语义等价旧实现按快照解析
	resp, err = f.ChatCompletionStreamRaw(context.Background(), 1, oldSrv.URL, "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "old", hit.Load(), "旧快照请求仍打旧上游")
}

func TestStreamRawContextTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.ChatCompletionStreamRaw(ctx, 1, srv.URL, "sk-test", []byte(`{"stream":true}`))
	require.Error(t, err)
}
