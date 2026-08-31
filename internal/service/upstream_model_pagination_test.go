// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// A few relays return only `page_token` as the continuation hint. It is a
// next-page cursor in that response shape, not an opaque field to ignore; the
// validator must fetch the later page or it can omit a manually usable model.
func TestFetchAdvertisedModelsFollowsPageTokenWithoutHasMore(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page_token") == "next-page" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-b"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}],"page_token":"next-page"}`))
	}))
	defer server.Close()

	models, code := fetchAdvertisedModels(context.Background(), server.Client(), server.URL, "relay-key")
	require.Empty(t, code)
	require.Equal(t, []string{"model-a", "model-b"}, models)
	require.Equal(t, []string{"/v1/models", "/v1/models?page_token=next-page"}, requests)
}
