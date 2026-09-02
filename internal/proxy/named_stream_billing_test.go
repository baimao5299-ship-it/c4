// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/stretchr/testify/require"
)

// A Claude-compatible relay can serve the OpenAI Chat endpoint while retaining
// named Anthropic SSE events. Before the event-aware parser, this successful
// response was recorded with zero tokens and therefore zero cost.
func TestNamedClaudeEventsOnChatEndpointAreBilled(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer up.Close()

	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, store, nil)
	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 100_000}}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{
		entries:  map[string]*domain.PriceEntry{"gpt-4o": proxyPricingEntry()},
		variants: map[string][]*domain.PriceVariant{"gpt-4o": proxyPricingVariants()},
	}, bal, rec)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	resp := httptest.NewRecorder()
	p.HandleChat(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	require.NoError(t, rec.Close(context.Background()))

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(3), store.logs[0].InputTokens)
	require.Equal(t, int64(5), store.logs[0].OutputTokens)
	require.Equal(t, int64(8), store.logs[0].TotalTokens)
	require.Equal(t, int64(130), store.logs[0].RawCost)
	require.Equal(t, int64(130), store.logs[0].Cost)
}
