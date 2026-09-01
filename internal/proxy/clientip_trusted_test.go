// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package proxy

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientIPTrustedProxyCIDRRejectsUntrustedPeer(t *testing.T) {
	r := ipReq("198.51.100.10:443", map[string]string{
		"CF-Connecting-IP": "203.0.113.9",
	})
	nets := parseTrustedProxyCIDRs([]string{"127.0.0.1/32", "::1/128"})

	ip, source, trusted := clientIPDetailsWithTrustedProxies(r, true, nets)
	require.Equal(t, "198.51.100.10", ip)
	require.Equal(t, clientIPSourceRemoteAddr, source)
	require.False(t, trusted)
}

func TestClientIPTrustedProxyCIDRAcceptsMatchingPeer(t *testing.T) {
	r := ipReq("127.0.0.1:443", map[string]string{
		"CF-Connecting-IP": "203.0.113.9",
	})
	nets := parseTrustedProxyCIDRs([]string{"127.0.0.1/32", "::1/128"})

	ip, source, trusted := clientIPDetailsWithTrustedProxies(r, true, nets)
	require.Equal(t, "203.0.113.9", ip)
	require.Equal(t, "cf_connecting_ip", source)
	require.True(t, trusted)
}

func TestClientIPTrustedProxyCIDRSupportsIPv6(t *testing.T) {
	r := ipReq("[::1]:443", map[string]string{
		"X-Real-IP": "2001:db8::9",
	})
	nets := parseTrustedProxyCIDRs([]string{"127.0.0.1/32", "::1/128"})

	ip, source, trusted := clientIPDetailsWithTrustedProxies(r, true, nets)
	require.Equal(t, "2001:db8::9", ip)
	require.Equal(t, "x_real_ip", source)
	require.True(t, trusted)
}

func TestClientIPTrustedProxyCIDREmptyPreservesLegacyBehavior(t *testing.T) {
	r := ipReq("198.51.100.10:443", map[string]string{
		"True-Client-IP": "203.0.113.9",
	})

	ip, source, trusted := clientIPDetailsWithTrustedProxies(r, true, nil)
	require.Equal(t, "203.0.113.9", ip)
	require.Equal(t, "true_client_ip", source)
	require.True(t, trusted)
}

func TestClientIPTrustedProxyCIDRMalformedPeerFallsBack(t *testing.T) {
	r := ipReq("not-an-ip", map[string]string{
		"X-Real-IP": "203.0.113.9",
	})
	nets := parseTrustedProxyCIDRs([]string{"127.0.0.1/32"})

	ip, source, trusted := clientIPDetailsWithTrustedProxies(r, true, nets)
	require.Equal(t, "not-an-ip", ip)
	require.Equal(t, clientIPSourceRemoteAddr, source)
	require.False(t, trusted)
}

func TestClientIPTrustedProxyCIDRDoesNotAffectDirectMode(t *testing.T) {
	r := ipReq("198.51.100.10:443", map[string]string{
		"CF-Connecting-IP": "203.0.113.9",
	})
	nets := parseTrustedProxyCIDRs([]string{"127.0.0.1/32"})

	ip, source, trusted := clientIPDetailsWithTrustedProxies(r, false, nets)
	require.Equal(t, "198.51.100.10", ip)
	require.Equal(t, clientIPSourceRemoteAddr, source)
	require.True(t, trusted)
}

func TestClientIPTrustedProxyCIDRHTTPRequestHasNoHeaderMutation(t *testing.T) {
	r := ipReq("127.0.0.1:443", map[string]string{
		"CF-Connecting-IP": "203.0.113.9",
	})
	before := r.Header.Get("CF-Connecting-IP")
	_, _, _ = clientIPDetailsWithTrustedProxies(r, true, parseTrustedProxyCIDRs([]string{"127.0.0.1/32"}))
	require.Equal(t, before, r.Header.Get("CF-Connecting-IP"))
	require.Equal(t, http.MethodPost, r.Method)
}
