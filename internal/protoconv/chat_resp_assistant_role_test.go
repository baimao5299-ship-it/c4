// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Responses only accepts output_text/refusal inside an assistant message, so a
// multi-turn chat history converted to Responses must not label assistant text
// as input_text. Doing so makes the upstream reject the whole request with
// invalid_value on input[N].content[0], which broke every second turn on relays
// routed through the Chat→Responses conversion.
func TestConvertRequestChatToRespUsesOutputTextForAssistantHistory(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello there"},
			{"role": "user", "content": [{"type": "text", "text": "follow up"}]},
			{"role": "assistant", "content": [{"type": "text", "text": "sure"}]}
		]
	}`)

	out, err := ConvertRequest(body, domain.ProtocolConvertChatToResp)
	require.NoError(t, err)

	input := arrOf(t, obj(t, out), "input")
	require.Len(t, input, 4)

	for _, tc := range []struct {
		at       int
		role     string
		textType string
	}{
		// String content and array text parts are separate code paths; cover both
		// for each role.
		{at: 0, role: "user", textType: "input_text"},
		{at: 1, role: "assistant", textType: "output_text"},
		{at: 2, role: "user", textType: "input_text"},
		{at: 3, role: "assistant", textType: "output_text"},
	} {
		item := input[tc.at].(map[string]any)
		require.Equal(t, tc.role, item["role"], "input[%d] role", tc.at)
		part := arrOf(t, item, "content")[0].(map[string]any)
		require.Equal(t, tc.textType, part["type"], "input[%d] (%s) text part type", tc.at, tc.role)
	}
}
