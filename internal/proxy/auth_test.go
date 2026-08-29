// SPDX-License-Identifier: AGPL-3.0-or-later

package proxy

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayKeyFromRequestBearerSchemeIsCaseInsensitive(t *testing.T) {
	for _, authorization := range []string{
		"bearer ck-1",
		"BEARER   ck-1",
		"  Bearer\tck-1  ",
	} {
		req := httptest.NewRequest("GET", "http://fixture.invalid", nil)
		req.Header.Set("Authorization", authorization)
		require.Equal(t, "ck-1", gatewayKeyFromRequest(req), authorization)
	}
}

func TestGatewayKeyFromRequestRejectsMalformedBearerAndFallsBackToAPIKey(t *testing.T) {
	req := httptest.NewRequest("GET", "http://fixture.invalid", nil)
	req.Header.Set("Authorization", "Bearer ck-1 extra")
	req.Header.Set("x-api-key", " ck-api ")
	require.Equal(t, "ck-api", gatewayKeyFromRequest(req))

	req.Header.Set("Authorization", "Bearer")
	req.Header.Set("x-api-key", "   ")
	require.Empty(t, gatewayKeyFromRequest(req))
}
