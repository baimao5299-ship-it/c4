// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
)

// assertLocalRejectionLog verifies the post-selection local failure contract:
// the client sees a 400, no provider request is sent, the scheduler slot is
// released, and the admin error log retains the selected account plus a clear
// reason that the upstream was never contacted.
func assertLocalRejectionLog(t *testing.T, p *Proxy, store *captureLogStore, rec *httptest.ResponseRecorder, hits *atomic.Int64, reason string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Zero(t, hits.Load(), "local rejection must not contact the upstream")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "local rejection must release the selected slot")
	require.NoError(t, p.rec.Close(context.Background()))
	if p.errlog != nil {
		require.NoError(t, p.errlog.Close(context.Background()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "local rejection must produce one admin error log")
	log := store.logs[0]
	require.Equal(t, int64(1), log.AccountID)
	require.Equal(t, domain.Err4xx, log.ErrorType)
	require.Equal(t, http.StatusBadRequest, log.StatusCode)
	require.Equal(t, "account", log.TargetKind)
	require.NotNil(t, log.ErrorMessage)
	require.Contains(t, *log.ErrorMessage, "upstream not contacted")
	require.Contains(t, *log.ErrorMessage, reason)
}

func callSelectedCaller(t *testing.T, p *Proxy, caller UpstreamCaller, format domain.RequestFormat, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	sel, err := p.sched.Select(10, format, "gpt-4o")
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(""))
	rec := httptest.NewRecorder()
	code, _, handled, callErr := caller.Call(context.Background(), rec, req, "req-local-reject", 10, time.Now(), sel, "sk-upstream", body, false)
	require.Equal(t, http.StatusBadRequest, code)
	require.True(t, handled)
	require.NoError(t, callErr)
	return rec
}

func TestLocalChatParameterRejectionIsLogged(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatOpenAIChat, store)

	// Call the selected caller directly with malformed JSON. The public handler
	// rejects malformed envelopes before selection; this isolates the caller's
	// post-selection guard and keeps the regression deterministic.
	rec := callSelectedCaller(t, p, p.callers[domain.FormatOpenAIChat], domain.FormatOpenAIChat, []byte("{"))

	assertLocalRejectionLog(t, p, store, rec, &hits, "invalid request body")
}

func TestLocalResponsesParameterRejectionIsLogged(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatOpenAIResponses, store)

	rec := callSelectedCaller(t, p, p.callers[domain.FormatOpenAIResponses], domain.FormatOpenAIResponses, []byte("{"))

	assertLocalRejectionLog(t, p, store, rec, &hits, "invalid request body")
}

func TestLocalAnthropicParameterRejectionIsLogged(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatAnthropic, store)

	rec := callSelectedCaller(t, p, p.callers[domain.FormatAnthropic], domain.FormatAnthropic, []byte("{"))

	assertLocalRejectionLog(t, p, store, rec, &hits, "invalid request body")
}

func TestProtocolConversionRejectionIsLogged(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up.Close()
	store := &captureLogStore{}
	// The target Responses route is selected, then conversion rejects the valid
	// JSON array before any SDK or HTTP call can occur.
	p := newConvertedTestProxyLogs(t, up.URL,
		[]domain.RequestFormat{domain.FormatOpenAIResponses},
		[]domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, store, 30*time.Second)
	p.errlog = usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, store, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`[1,2,3]`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	assertLocalRejectionLog(t, p, store, rec, &hits, "protocol conversion failed")
}
