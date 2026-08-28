// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"net/http"
	"testing"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/pkg/httpx"
	"github.com/stretchr/testify/require"
)

func TestInstallDefaultHTTPTransportUsesExplicitDirectMode(t *testing.T) {
	previous := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = previous })

	installDefaultHTTPTransport(config.UpstreamConfig{DialTimeout: 2}, httpx.ProxyFuncs{})
	transport, ok := http.DefaultClient.Transport.(*http.Transport)
	require.True(t, ok)
	require.Nil(t, transport.Proxy, "empty proxy_url must be deterministic direct mode")
}
