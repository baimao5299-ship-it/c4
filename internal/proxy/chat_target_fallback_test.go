// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/protoconv"
)

type chatTargetUpstream struct {
	mu       sync.Mutex
	paths    []string
	omitDone bool
}

func (u *chatTargetUpstream) snapshotPaths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

func (u *chatTargetUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()

		switch r.URL.Path {
		case "/v1/responses", "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, `{"error":{"message":"this endpoint is not supported"}}`)
		case "/v1/chat/completions":
			if payload["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n")
				if !u.omitDone {
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestAutoNegotiationResponsesUsesNativeThenMessagesThenChat(t *testing.T) {
	up := &chatTargetUpstream{}
	srv := up.server(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{
		domain.FormatOpenAIResponses, domain.FormatAnthropic, domain.FormatOpenAIChat,
	}, []domain.ProtocolConvert{domain.ProtocolConvertAuto})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"/v1/responses", "/v1/messages", "/v1/chat/completions"}, up.snapshotPaths())
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "response", got["object"])
	require.Equal(t, "completed", got["status"])
}

func TestAutoNegotiationMessagesUsesNativeThenResponsesThenChat(t *testing.T) {
	up := &chatTargetUpstream{}
	srv := up.server(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{
		domain.FormatAnthropic, domain.FormatOpenAIResponses, domain.FormatOpenAIChat,
	}, []domain.ProtocolConvert{domain.ProtocolConvertAuto})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"}, up.snapshotPaths())
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "message", got["type"])
	require.Equal(t, "assistant", got["role"])
}

func TestAutoNegotiationUsesChatDirectlyWhenOnlyChatRouteExists(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		body   string
		handle func(*Proxy, http.ResponseWriter, *http.Request)
	}{
		{name: "responses", path: "/v1/responses", body: `{"model":"gpt-4o","input":"hi"}`, handle: (*Proxy).HandleResponses},
		{name: "messages", path: "/v1/messages", body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`, handle: (*Proxy).HandleAnthropic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &chatTargetUpstream{}
			srv := up.server(t)
			defer srv.Close()
			p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat}, []domain.ProtocolConvert{domain.ProtocolConvertAuto})
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			tc.handle(p, rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, []string{"/v1/chat/completions"}, up.snapshotPaths())
		})
	}
}

func TestAutoNegotiationChatTargetStreamingPreservesClientProtocol(t *testing.T) {
	for _, tc := range []struct {
		name      string
		path      string
		body      string
		handle    func(*Proxy, http.ResponseWriter, *http.Request)
		completed string
	}{
		{name: "responses", path: "/v1/responses", body: `{"model":"gpt-4o","input":"hi","stream":true}`, handle: (*Proxy).HandleResponses, completed: "event: response.completed"},
		{name: "messages", path: "/v1/messages", body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`, handle: (*Proxy).HandleAnthropic, completed: "event: message_stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &chatTargetUpstream{}
			srv := up.server(t)
			defer srv.Close()
			p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat}, []domain.ProtocolConvert{domain.ProtocolConvertAuto})
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			tc.handle(p, rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), tc.completed)
			require.NotContains(t, rec.Body.String(), "[DONE]")
		})
	}
}

func TestAutoNegotiationChatTargetStreamingCompletesWithoutDoneSentinel(t *testing.T) {
	up := &chatTargetUpstream{omitDone: true}
	srv := up.server(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat}, []domain.ProtocolConvert{domain.ProtocolConvertAuto})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.completed"))
	require.NotContains(t, rec.Body.String(), "[DONE]")
}

func TestAutoConversionCandidatesExcludeNonConversationFormats(t *testing.T) {
	for _, format := range []domain.RequestFormat{
		domain.FormatOpenAIResponsesWS,
		domain.FormatOpenAIImages,
		domain.FormatOpenAISearch,
	} {
		require.Empty(t, conversionCandidates([]domain.ProtocolConvert{domain.ProtocolConvertAuto}, format))
	}
}

func TestProtocolFallbackPreservesAttemptedAccounts(t *testing.T) {
	attempted := []int64{11, 22, 33, 22}

	require.True(t, containsAttempted(attempted, 11))
	require.True(t, containsAttempted(attempted, 33))
	require.False(t, containsAttempted(attempted, 44))

	// Only the member that just failed the first protocol may be retried on an
	// alternate protocol. Every other previously attempted member stays out of
	// the candidate set, and duplicate history entries remain collapsed.
	require.Equal(t, []int64{11, 33}, excludeAttemptedExcept(attempted, 22))
	require.Equal(t, []int64{22, 33}, excludeAttemptedExcept(attempted, 11))
}

func TestRuntimeOnlyChatTargetsAreIgnoredWithoutAutoMode(t *testing.T) {
	require.Empty(t, conversionCandidates([]domain.ProtocolConvert{
		protoconv.AutoResponsesToChat,
		protoconv.AutoMessagesToChat,
	}, domain.FormatOpenAIResponses))
}
