// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package proxy

import (
	"errors"
	"net/http"
	"testing"
)

func TestIsProtocolCapabilityError(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		err  error
		want bool
	}{
		{name: "method", code: http.StatusMethodNotAllowed, want: true},
		{name: "media type", code: http.StatusUnsupportedMediaType, want: true},
		{name: "not implemented", code: http.StatusNotImplemented, want: true},
		{name: "missing path", code: http.StatusNotFound, want: true},
		{name: "unsupported 400", code: http.StatusBadRequest, body: `{"error":{"message":"Responses API is not supported"}}`, want: true},
		{name: "unsupported 422", code: http.StatusUnprocessableEntity, body: `{"error":{"message":"invalid content-type for this endpoint"}}`, want: true},
		{name: "bad model", code: http.StatusBadRequest, body: `{"error":{"message":"model not found"}}`, want: false},
		{name: "auth", code: http.StatusUnauthorized, body: `{"error":{"message":"unsupported key"}}`, want: false},
		{name: "json decode", code: 0, err: errors.New("invalid character 'x' looking for beginning of value"), want: false},
		{name: "network", code: 0, err: errors.New("dial tcp: connection refused"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProtocolCapabilityError(tc.code, []byte(tc.body), tc.err); got != tc.want {
				t.Fatalf("isProtocolCapabilityError() = %v, want %v", got, tc.want)
			}
		})
	}
}
