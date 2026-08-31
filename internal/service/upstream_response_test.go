// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsJSONObjectResponseRequiresProviderEnvelope(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"status":"ok"}`),
		[]byte(`{"message":"ok"}`),
		[]byte(`{"error":null}`),
		[]byte(`{"object":"error"}`),
		[]byte(`{"object":"status"}`),
	} {
		require.Falsef(t, isJSONObjectResponse(body), "placeholder body %q must be rejected", body)
	}
	for _, body := range [][]byte{
		[]byte(`{"id":"resp_1"}`),
		[]byte(`{"error":null,"id":"resp_2"}`),
		[]byte(`{"object":"chat.completion"}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_3"}}`),
		[]byte(`{"choices":[{"index":0}]}`),
		[]byte(`{"output":[{"type":"message"}]}`),
		[]byte(`{"data":[{"id":"item"}]}`),
	} {
		require.Truef(t, isJSONObjectResponse(body), "provider envelope %q must be accepted", body)
	}
	require.False(t, isJSONObjectResponse([]byte(`{"data":[]}`)), "an empty model catalogue is not a completion response")
	require.True(t, isJSONObjectResponse([]byte(`{"choices":[]}`)), "an empty completion envelope is still a valid response")
	require.True(t, isJSONObjectResponse([]byte(`{"output":[]}`)), "an empty Responses envelope is still a valid response")
}

func TestIsUpstreamSuccessResponseAcceptsSSEFrames(t *testing.T) {
	for _, body := range [][]byte{
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n"),
		[]byte("data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\"}\n\ndata: [DONE]\n\n"),
	} {
		require.Truef(t, isUpstreamSuccessResponse(body), "SSE body %q must be accepted", body)
	}
	for _, body := range [][]byte{
		[]byte("event: ping\ndata: {}\n\n"),
		[]byte("<html>proxy login</html>"),
	} {
		require.Falsef(t, isUpstreamSuccessResponse(body), "non-provider body %q must be rejected", body)
	}
}

func TestClassifyUpstreamTestErrorMapsTimeoutAndAllServerErrors(t *testing.T) {
	require.Equal(t, "timeout", classifyUpstreamTestError(nil, http.StatusRequestTimeout, nil))
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		require.Equalf(t, "upstream", classifyUpstreamTestError(nil, status, nil), "status %d", status)
	}
}
