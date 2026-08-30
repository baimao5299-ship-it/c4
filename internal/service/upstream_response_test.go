// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
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
		[]byte(`{"object":"chat.completion"}`),
		[]byte(`{"choices":[{"index":0}]}`),
		[]byte(`{"output":[{"type":"message"}]}`),
		[]byte(`{"data":[{"id":"item"}]}`),
	} {
		require.Truef(t, isJSONObjectResponse(body), "provider envelope %q must be accepted", body)
	}
	for _, body := range [][]byte{
		[]byte(`{"choices":[]}`),
		[]byte(`{"output":[]}`),
		[]byte(`{"data":[]}`),
	} {
		require.Falsef(t, isJSONObjectResponse(body), "empty provider envelope %q must be rejected", body)
	}
}
