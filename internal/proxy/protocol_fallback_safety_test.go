// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package proxy

import (
	"net/http"
	"testing"
)

func TestIsProtocolCapabilityErrorDoesNotRetryProviderJSON404(t *testing.T) {
	for _, message := range []string{
		`{"error":{"message":"quota exceeded"}}`,
		`{"error":{"message":"authentication failed"}}`,
		`{"error":{"message":"provider unavailable"}}`,
		`{"error":{"message":"invalid request"}}`,
	} {
		if isProtocolCapabilityError(http.StatusNotFound, []byte(message), nil) {
			t.Fatalf("provider error must not trigger a second request: %s", message)
		}
	}
}

func TestIsProtocolCapabilityErrorKeepsStructuredRoute404Compatibility(t *testing.T) {
	for _, message := range []string{
		`{"error":{"message":"Not Found"}}`,
		`{"error":{"message":"endpoint not found"}}`,
		`{"error":{"message":"unknown path /v1/responses"}}`,
	} {
		if !isProtocolCapabilityError(http.StatusNotFound, []byte(message), nil) {
			t.Fatalf("route error must trigger compatibility fallback: %s", message)
		}
	}
}
