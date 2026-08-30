// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestAIRouterChatBasePathAlias keeps clients working when their SDK appends
// /chat/completions to a base URL that does not include /v1. The alias must use
// exactly the same authenticated forwarding path as the canonical endpoint.
func TestAIRouterChatBasePathAlias(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	AIRouter(p).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "base-path alias must forward: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"object":"chat.completion"`)
}

// TestAIRouterCompatibilityAliasesMethodGuards verifies that aliases are
// registered without widening their method surface. A wrong method remains a
// 405, while GET /responses is reserved for WebSocket upgrades only.
func TestAIRouterCompatibilityAliasesMethodGuards(t *testing.T) {
	r := AIRouter(nil)
	for _, tc := range []struct {
		name   string
		path   string
		method string
	}{
		{name: "chat", path: "/chat/completions", method: http.MethodGet},
		{name: "responses", path: "/responses", method: http.MethodGet},
		{name: "messages", path: "/messages", method: http.MethodGet},
		{name: "models", path: "/models", method: http.MethodPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		})
	}

	// The responses GET handler is intentionally present for WS. Without an
	// upgrade it returns the same 405 contract rather than calling the proxy.
	req := httptest.NewRequest(http.MethodGet, "/responses", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestAIRouterResponsesAliasWebSocket confirms that the unversioned alias is
// dispatched to the existing Responses WebSocket flow, not to HTTP handling.
func TestAIRouterResponsesAliasWebSocket(t *testing.T) {
	up := fakeResponsesWS(t, &fakeWSHooks{frameLimit: 1})
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatOpenAIResponsesWS, store)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/responses"
	c, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer ck-1"},
			"X-Api-Key":     {"ck-1"},
		},
	})
	require.NoError(t, err, "responses alias must complete WS handshake")
	defer c.CloseNow()

	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	frame := readResponsesWSFrame(t, c)
	require.Contains(t, string(frame), `"type":"response.created"`)
}
