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
		[]byte(`{"id":"resp_failed","type":"response.failed","status":"failed"}`),
		[]byte(`{"id":"resp_false","success":false}`),
		[]byte(`{"id":"resp_pending","type":"response.created","status":"in_progress"}`),
		[]byte(`{"data":{"error":{"message":"provider unavailable"}}}`),
		[]byte(`{"result":{"id":"failed","type":"response.failed"}}`),
		[]byte(`{"data":{"id":"pending","type":"response.created"}}`),
		[]byte(`{"id":"outer","payload":{"status":"failed"}}`),
		[]byte(`{"id":"outer","body":{"type":"response.created"}}`),
		[]byte(`{"id":"request-1","status":"ok"}`),
		[]byte(`{"data":{"message":"login required"}}`),
		[]byte(`{"data":[{"id":"model-list-item"}]}`),
	} {
		require.Falsef(t, isJSONObjectResponse(body), "placeholder body %q must be rejected", body)
	}
	for _, body := range [][]byte{
		[]byte(`{"id":"resp_1","object":"response"}`),
		[]byte(`{"error":null,"id":"resp_2","object":"response"}`),
		[]byte(`{"object":"chat.completion"}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_3"}}`),
		[]byte(`{"choices":[{"index":0}]}`),
		[]byte(`{"output":[{"type":"message"}]}`),
		[]byte(`{"data":[{"url":"https://image.example/result.png"}]}`),
	} {
		require.Truef(t, isJSONObjectResponse(body), "provider envelope %q must be accepted", body)
	}
	require.False(t, isJSONObjectResponse([]byte(`{"data":[]}`)), "an empty model catalogue is not a completion response")
	require.True(t, isJSONObjectResponse([]byte(`{"choices":[]}`)), "an empty completion envelope is still a valid response")
	require.True(t, isJSONObjectResponse([]byte(`{"output":[]}`)), "an empty Responses envelope is still a valid response")
}

func TestIsJSONObjectResponseWaitsForMeaningfulChatChunk(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"id":"chat-1","object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`),
		[]byte(`{"choices":[{"delta":{"role":"assistant"}}]}`),
		[]byte(`{"object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":""}]}`),
		[]byte(`{"object":"chat.completion.chunk","choices":[{"delta":{"content":""},"finish_reason":null}]}`),
	} {
		require.Falsef(t, isJSONObjectResponse(body), "non-meaningful chat chunk %q must keep waiting", body)
	}

	for _, body := range [][]byte{
		[]byte(`{"object":"chat.completion.chunk","choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`),
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1"}]}}]}`),
		[]byte(`{"object":"chat.completion.chunk","choices":[{"delta":{"function_call":{"name":"lookup"}}}]}`),
		[]byte(`{"object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}]}`),
	} {
		require.Truef(t, isJSONObjectResponse(body), "meaningful chat chunk %q must prove success", body)
	}
}

func TestIsUpstreamSuccessResponseAcceptsSSEFrames(t *testing.T) {
	for _, body := range [][]byte{
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n"),
		[]byte("data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"),
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

func TestSSEEventNamePreventsGenericErrorPayloadFromLookingSuccessful(t *testing.T) {
	body := []byte("event: error\ndata: {\"id\":\"err-1\",\"message\":\"rate limit exceeded\"}\n\n")
	require.True(t, isUpstreamFailureResponse(body))
	require.False(t, isUpstreamSuccessResponse(body))
}

func TestClassifyUpstreamTestErrorMapsTimeoutAndAllServerErrors(t *testing.T) {
	require.Equal(t, "timeout", classifyUpstreamTestError(nil, http.StatusRequestTimeout, nil))
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		require.Equalf(t, "upstream", classifyUpstreamTestError(nil, status, nil), "status %d", status)
	}
}
