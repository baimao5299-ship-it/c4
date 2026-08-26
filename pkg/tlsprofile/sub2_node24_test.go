// SPDX-License-Identifier: AGPL-3.0-or-later
package tlsprofile

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/stretchr/testify/require"
)

func TestSub2Node24DialTLSContextHonorsHandshakeTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() { _, _ = io.Copy(io.Discard, serverConn) }()

	dialTLS := NewSub2Node24DialTLSContext(func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}, 25*time.Millisecond)
	started := time.Now()
	conn, err := dialTLS(context.Background(), "tcp", "upstream.example:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("TLS dial unexpectedly completed without a peer handshake")
	}
	if err == nil {
		t.Fatal("TLS dial unexpectedly completed without a peer handshake")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("TLS handshake timeout was not applied; elapsed=%s", elapsed)
	}
}

func TestSub2Node24DialTLSContextCompletesRealHTTPSHandshake(t *testing.T) {
	var protocol string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol = r.Proto
		_, _ = io.WriteString(w, "ok")
	}))
	server.StartTLS()
	defer server.Close()
	server.TLS.NextProtos = []string{"http/1.1"}

	dialer := &net.Dialer{Timeout: time.Second}
	dialTLS := newSub2Node24DialTLSContext(dialer.DialContext, time.Second, func(host string) *utls.Config {
		return &utls.Config{ServerName: host, InsecureSkipVerify: true} // local httptest certificate
	})
	transport := &http.Transport{DialTLSContext: dialTLS, ForceAttemptHTTP2: false}
	client := &http.Client{Transport: transport}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "ok", string(body))
	require.Equal(t, "HTTP/1.1", protocol)
}

func TestSub2Node24DialTLSContextCompletesWebSocketUpgrade(t *testing.T) {
	var protocol string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol = r.Proto
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "upgrade required", http.StatusBadRequest)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
	}))
	server.StartTLS()
	defer server.Close()
	server.TLS.NextProtos = []string{"http/1.1"}

	dialer := &net.Dialer{Timeout: time.Second}
	dialTLS := newSub2Node24DialTLSContext(dialer.DialContext, time.Second, func(host string) *utls.Config {
		return &utls.Config{ServerName: host, InsecureSkipVerify: true} // local httptest certificate
	})
	transport := &http.Transport{DialTLSContext: dialTLS, ForceAttemptHTTP2: false}
	client := &http.Client{Transport: transport}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.Equal(t, "HTTP/1.1", protocol)
	require.NoError(t, resp.Body.Close())
}
