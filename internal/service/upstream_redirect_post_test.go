// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// net/http normally rewrites a POST to GET for 301/302/303 redirects. Probe
// endpoints are request-shape sensitive, so a same-origin route alias must
// preserve the method, body, and content type before the model is classified.
func TestSendUpstreamTestRequestPreservesPOSTAcrossLegacyRedirects(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var first atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if first.CompareAndSwap(false, true) {
					require.Equal(t, http.MethodPost, r.Method)
					http.Redirect(w, r, "/v1/responses/", status)
					return
				}
				require.Equal(t, "/v1/responses/", r.URL.Path)
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "application/json", r.Header.Get("Content-Type"))
				require.Equal(t, "Bearer relay-key", r.Header.Get("Authorization"))
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, "manual-model", body["model"])
				require.Equal(t, "hi", body["input"])
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"redirect-response","object":"response"}`))
			}))
			defer server.Close()

			client := server.Client()
			client.CheckRedirect = upstreamCheckRedirect
			statusCode, err := sendUpstreamTestRequest(context.Background(), client, server.URL+"/v1/responses", "relay-key", "manual-model", false)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, statusCode)
		})
	}
}
