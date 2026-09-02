// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package protoconv

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestConvertResponseRejectsApplicationFailureEnvelopes(t *testing.T) {
	directions := []domain.ProtocolConvert{
		domain.ProtocolConvertChatToResp,
		domain.ProtocolConvertMessToResp,
		domain.ProtocolConvertRespToMess,
		domain.ProtocolConvertChatToMess,
		AutoResponsesToChat,
		AutoMessagesToChat,
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "top level error",
			body: `{"error":{"type":"rate_limit","message":"provider busy"}}`,
			want: "provider busy",
		},
		{
			name: "failed response",
			body: `{"status":"failed","error":{"code":"server_error","message":"generation failed"}}`,
			want: "generation failed",
		},
		{
			name: "cancelled response",
			body: `{"status":"cancelled","message":"request cancelled"}`,
			want: "request cancelled",
		},
		{
			name: "nested response failure",
			body: `{"response":{"status":"failed","error":{"message":"nested failure"}}}`,
			want: "nested failure",
		},
		{
			name: "anthropic error type",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`,
			want: "bad request",
		},
		{
			name: "data wrapper failure",
			body: `{"data":{"status":"failed","error":{"message":"wrapped failure"}}}`,
			want: "wrapped failure",
		},
		{
			name: "result wrapper error",
			body: `{"success":false,"result":{"type":"response.failed","message":"result failed"}}`,
			want: "result failed",
		},
		{
			name: "nested payload wrapper",
			body: `{"payload":{"body":{"error":{"code":"provider_error","message":"deep failure"}}}}`,
			want: "deep failure",
		},
		{
			name: "explicit unsuccessful flag",
			body: `{"success":false,"message":"request rejected"}`,
			want: "request rejected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, dir := range directions {
				out, err := ConvertResponse([]byte(tc.body), dir)
				require.Nil(t, out)
				var failure *UpstreamResponseError
				require.ErrorAs(t, err, &failure, "direction=%s", dir)
				require.Equal(t, 502, failure.StatusCode())
				require.Contains(t, err.Error(), tc.want, "direction=%s", dir)
			}
		})
	}
}

func TestDetectUpstreamResponseFailureDoesNotWalkProviderContentMetadata(t *testing.T) {
	for _, body := range []string{
		`{"object":"response","status":"completed","output":[{"type":"message","metadata":{"error":"not a response failure"}}]}`,
		`{"object":"chat.completion","choices":[{"message":{"content":"ok","metadata":{"error":{"message":"not a response failure"}}}}]}`,
	} {
		require.NoError(t, detectUpstreamResponseFailure([]byte(body)), "body=%s", body)
	}
}

func TestConvertResponseKeepsSuccessfulFailureLookingFieldsNested(t *testing.T) {
	// A tool/message payload may legitimately contain an `error` field. Only the
	// response envelope itself is inspected, so this remains a normal conversion.
	body := []byte(`{"id":"rsp_1","status":"completed","model":"m","output":[{"type":"message","content":[{"type":"output_text","text":"error details"}],"metadata":{"error":"not a failure"}}]}`)
	out, err := ConvertResponse(body, domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	require.NotEmpty(t, out)
}

func TestDetectUpstreamResponseFailureIgnoresNullAndSuccess(t *testing.T) {
	for _, body := range []string{
		`{"status":"completed","error":null}`,
		`{"success":true,"message":"ok"}`,
		`{"ok":true,"object":"response"}`,
		`{"status":"incomplete","message":"truncated"}`,
		`{"status":"in_progress"}`,
	} {
		require.NoError(t, detectUpstreamResponseFailure([]byte(body)), "body=%s", body)
	}
	var failure *UpstreamResponseError
	require.False(t, errors.As(detectUpstreamResponseFailure([]byte(`{"status":"completed"}`)), &failure))
}
