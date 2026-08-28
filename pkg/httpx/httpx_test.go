// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpx

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientReusesTransport(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := NewClient(TransportConfig{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     10 * time.Second,
		DialTimeout:         5 * time.Second,
		ForceHTTP2:          false,
	})
	for i := 0; i < 3; i++ {
		resp, err := c.Get(srv.URL)
		require.NoError(t, err)
		resp.Body.Close()
	}
	require.Equal(t, 3, hits)
}

// TestTransportProxyDefaultsToDirect C2-1 默认直连：TransportConfig.Proxy 零值
// （nil）→ 产物 transport 的 Proxy 为 nil——不再隐式装配
// http.ProxyFromEnvironment（HTTP_PROXY 设置即静默改道上游请求，含凭据）。
func TestTransportProxyDefaultsToDirect(t *testing.T) {
	tr := NewTransport(TransportConfig{})
	require.Nil(t, tr.Proxy, "默认直连：不得隐式装配 http.ProxyFromEnvironment")
}

func TestTransportTLSConvergenceDisabledKeepsStandardTLS(t *testing.T) {
	tr := NewTransport(TransportConfig{ForceHTTP2: true})
	require.Nil(t, tr.DialTLSContext)
	require.True(t, tr.ForceAttemptHTTP2)
}

func TestTransportTLSConvergenceUsesSub2Profile(t *testing.T) {
	tr := NewTransport(TransportConfig{ForceHTTP2: true, TLSConvergence: true})
	require.NotNil(t, tr.DialTLSContext)
	require.False(t, tr.ForceAttemptHTTP2, "Sub2 Node.js profile only advertises HTTP/1.1")
}

// TestTransportNilProxyIgnoresEnv C2-1 Proxy=nil 直连：即便 HTTP_PROXY 环境变量
// 指向代理，上游请求仍直连目标（代理零命中），行为不随部署环境漂移。
func TestTransportNilProxyIgnoresEnv(t *testing.T) {
	var proxyHits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits++
	}))
	defer proxy.Close()
	// 全形态环境代理变量都指向代理；NO_PROXY 置空避免宿主环境豁免干扰。
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	// Proxy 不设 = nil 直连（显式零值，无代理依赖断言）。
	c := NewClient(TransportConfig{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     10 * time.Second,
		DialTimeout:         5 * time.Second,
		ForceHTTP2:          false,
	})
	resp, err := c.Get(target.URL)
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, 1, targetHits)
	require.Zero(t, proxyHits, "Proxy=nil 时 HTTP_PROXY 环境变量不得改道上游请求")
}

// TestTransportExplicitProxy C2-1 Proxy 显式设置生效：请求经配置的代理转发
// （绝对 URI 形式），目标不被直连。
func TestTransportExplicitProxy(t *testing.T) {
	var proxyHits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		require.True(t, r.URL.IsAbs(), "经代理的请求应为绝对 URI（代理转发形态）")
		_, _ = w.Write([]byte("via-proxy"))
	}))
	defer proxy.Close()

	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	// 显式代理来自配置字段；宿主环境代理变量置空避免歧义。
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	c := NewClient(TransportConfig{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     10 * time.Second,
		DialTimeout:         5 * time.Second,
		ForceHTTP2:          false,
		Proxy:               func(*http.Request) (*url.URL, error) { return proxyURL, nil },
	})
	resp, err := c.Get(target.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, "via-proxy", string(body))
	require.Equal(t, 1, proxyHits)
	require.Zero(t, targetHits)
}

func TestParseProxySchemes(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		proxy  bool
		dial   bool
		scheme string
	}{
		{raw: "", scheme: ""},
		{raw: "http://127.0.0.1:7897", proxy: true, scheme: "http"},
		{raw: "https://127.0.0.1:7897", proxy: true, scheme: "https"},
		{raw: "socks5h://127.0.0.1:7898", dial: true, scheme: "socks5h"},
	} {
		got, err := ParseProxy(tc.raw)
		require.NoError(t, err)
		require.Equal(t, tc.scheme, got.Scheme)
		require.Equal(t, tc.proxy, got.Proxy != nil)
		require.Equal(t, tc.dial, got.DialContext != nil)
	}
}

func TestParseProxyRejectsAmbiguousOrLocalDNSModes(t *testing.T) {
	for _, raw := range []string{
		"socks5://127.0.0.1:7898",
		"http://127.0.0.1",
		"http://127.0.0.1:7897/path",
		"http://127.0.0.1:7897?x=1",
		"http://user:pass@127.0.0.1:7897",
		"http://127.0.0.1:not-a-port",
		"http://127.0.0.1:0",
		"ftp://127.0.0.1:21",
	} {
		_, err := ParseProxy(raw)
		require.Error(t, err, raw)
	}
}

func TestSocks5hDialSendsHostnameToProxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	seen := make(chan string, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(conn, greeting[:]); err != nil {
			serverErr <- err
			return
		}
		_, _ = conn.Write([]byte{0x05, 0x00})
		header := make([]byte, 5)
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			serverErr <- err
			return
		}
		if header[3] != 0x03 {
			serverErr <- io.ErrUnexpectedEOF
			return
		}
		host := make([]byte, int(header[4])+2)
		if _, err := io.ReadFull(conn, host); err != nil {
			serverErr <- err
			return
		}
		port := binary.BigEndian.Uint16(host[len(host)-2:])
		seen <- string(host[:len(host)-2]) + ":" + strconv.Itoa(int(port))
		_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		serverErr <- nil
	}()

	funcs, err := ParseProxyWithTimeout("socks5h://"+listener.Addr().String(), time.Second)
	require.NoError(t, err)
	conn, err := funcs.DialContext(context.Background(), "tcp", "chatgpt.example:443")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.Equal(t, "chatgpt.example:443", <-seen)
	require.NoError(t, <-serverErr)
}

func TestSocks5hDialSupportsCredentials(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	seen := make(chan [2]string, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		greeting := make([]byte, 4)
		if _, readErr := io.ReadFull(conn, greeting); readErr != nil {
			serverErr <- readErr
			return
		}
		if greeting[0] != 0x05 || greeting[1] != 0x02 || greeting[2] != 0x00 || greeting[3] != 0x02 {
			serverErr <- io.ErrUnexpectedEOF
			return
		}
		_, _ = conn.Write([]byte{0x05, 0x02})
		authHead := make([]byte, 2)
		if _, readErr := io.ReadFull(conn, authHead); readErr != nil {
			serverErr <- readErr
			return
		}
		user := make([]byte, int(authHead[1]))
		if _, readErr := io.ReadFull(conn, user); readErr != nil {
			serverErr <- readErr
			return
		}
		var passwordLen [1]byte
		if _, readErr := io.ReadFull(conn, passwordLen[:]); readErr != nil {
			serverErr <- readErr
			return
		}
		password := make([]byte, int(passwordLen[0]))
		if _, readErr := io.ReadFull(conn, password); readErr != nil {
			serverErr <- readErr
			return
		}
		seen <- [2]string{string(user), string(password)}
		_, _ = conn.Write([]byte{0x01, 0x00})
		requestHead := make([]byte, 5)
		if _, readErr := io.ReadFull(conn, requestHead); readErr != nil {
			serverErr <- readErr
			return
		}
		target := make([]byte, int(requestHead[4])+2)
		if _, readErr := io.ReadFull(conn, target); readErr != nil {
			serverErr <- readErr
			return
		}
		_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		serverErr <- nil
	}()

	u := &url.URL{Scheme: "socks5h", Host: listener.Addr().String(), User: url.UserPassword("fixture-user", "fixture-pass")}
	funcs, err := ParseProxy(u.String())
	require.NoError(t, err)
	conn, err := funcs.DialContext(context.Background(), "tcp", "chatgpt.example:443")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.Equal(t, [2]string{"fixture-user", "fixture-pass"}, <-seen)
	require.NoError(t, <-serverErr)
	require.Equal(t, "socks5h://"+listener.Addr().String(), ProxySummary(u.String()))
}

func TestSocks5hDialHonorsContextCancellationDuringHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	serverReady := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		greeting := make([]byte, 3)
		if _, readErr := io.ReadFull(conn, greeting); readErr != nil {
			return
		}
		close(serverReady)
		// Keep the SOCKS greeting blocked. DialContext must close this
		// connection when the request context is canceled.
		var response [2]byte
		_, _ = io.ReadFull(conn, response[:])
	}()

	funcs, err := ParseProxyWithTimeout("socks5h://"+listener.Addr().String(), 5*time.Second)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, dialErr := funcs.DialContext(ctx, "tcp", "chatgpt.example:443")
		if conn != nil {
			_ = conn.Close()
		}
		result <- dialErr
	}()

	select {
	case <-serverReady:
	case <-time.After(time.Second):
		t.Fatal("SOCKS5 greeting was not sent")
	}
	cancel()
	select {
	case dialErr := <-result:
		require.ErrorIs(t, dialErr, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("DialContext did not stop after context cancellation")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("SOCKS5 connection was not closed after cancellation")
	}
}
