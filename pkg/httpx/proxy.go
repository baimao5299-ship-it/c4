package httpx

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProxyFuncs is the explicit upstream proxy decision. Exactly one of Proxy or
// DialContext is set for a configured proxy; both are nil for direct mode.
type ProxyFuncs struct {
	Proxy       func(*http.Request) (*url.URL, error)
	DialContext func(context.Context, string, string) (net.Conn, error)
	Scheme      string
}

// ParseProxy parses a user-supplied proxy URL. An empty value means direct
// mode. socks5h keeps hostname resolution at the proxy, avoiding polluted
// container DNS; socks5 (local DNS) is intentionally rejected.
func ParseProxy(raw string) (ProxyFuncs, error) {
	return ParseProxyWithTimeout(raw, 10*time.Second)
}

// ParseProxyWithTimeout is ParseProxy with an explicit first-hop dial timeout.
// The timeout is applied only to the proxy connection and handshake; the
// request context still controls the complete operation.
func ParseProxyWithTimeout(raw string, dialTimeout time.Duration) (ProxyFuncs, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ProxyFuncs{}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.Port() == "" {
		return ProxyFuncs{}, fmt.Errorf("proxy URL must include scheme, host, and port")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return ProxyFuncs{}, fmt.Errorf("proxy URL must not contain a path, query, or fragment")
	}
	scheme := strings.ToLower(u.Scheme)
	// SOCKS5h credentials are consumed by our explicit handshake. HTTP(S)
	// proxy credentials stay rejected because net/http would manage them from
	// the URL and they could be copied into diagnostics or redirects.
	if u.User != nil && scheme != "socks5h" {
		return ProxyFuncs{}, fmt.Errorf("proxy URL must not contain embedded credentials")
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || port == 0 {
		return ProxyFuncs{}, fmt.Errorf("proxy URL port must be a number from 1 to 65535")
	}
	switch scheme {
	case "http", "https":
		return ProxyFuncs{Proxy: http.ProxyURL(u), Scheme: scheme}, nil
	case "socks5h":
		if u.Port() == "" {
			return ProxyFuncs{}, fmt.Errorf("socks5h proxy URL must include port")
		}
		if dialTimeout <= 0 {
			dialTimeout = 10 * time.Second
		}
		return ProxyFuncs{DialContext: newSocks5Dialer(u, dialTimeout).DialContext, Scheme: scheme}, nil
	case "socks5":
		return ProxyFuncs{}, fmt.Errorf("socks5 is not supported; use socks5h to keep DNS at the proxy")
	default:
		return ProxyFuncs{}, fmt.Errorf("unsupported proxy scheme %q (use http, https, or socks5h)", u.Scheme)
	}
}

// ProxySummary returns a stable display value without userinfo. It is intended
// for diagnostics and status responses; the raw URL must never be logged or
// sent back to clients because proxy credentials may be embedded in it.
func ProxySummary(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "direct"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "invalid"
	}
	u.User = nil
	return u.String()
}

type socks5Dialer struct {
	address  string
	username string
	password string
	timeout  time.Duration
}

func newSocks5Dialer(u *url.URL, timeout time.Duration) *socks5Dialer {
	d := &socks5Dialer{address: u.Host, timeout: timeout}
	if u.User != nil {
		d.username = u.User.Username()
		d.password, _ = u.User.Password()
	}
	return d
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, address string) (out net.Conn, outErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if network != "tcp" {
		return nil, fmt.Errorf("socks5h only supports tcp")
	}
	conn, err := (&net.Dialer{Timeout: d.timeout, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", d.address)
	if err != nil {
		return nil, err
	}
	// net.Conn reads and writes do not consistently wake up for context
	// cancellation. Keep a short-lived watcher for the SOCKS handshake and
	// close the connection on cancellation so a blocked handshake returns
	// promptly instead of waiting for the fixed dial deadline.
	stopCancel := make(chan struct{})
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopCancel:
		}
	}()
	defer func() {
		close(stopCancel)
		<-cancelDone
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = conn.Close()
			out = nil
			outErr = ctxErr
		} else if outErr != nil {
			_ = conn.Close()
		}
	}()
	deadline := time.Now().Add(d.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	closeOnError := func(e error) (net.Conn, error) { _ = conn.Close(); return nil, e }
	methods := []byte{0x00}
	if d.username != "" {
		methods = append(methods, 0x02)
	}
	if _, err = conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return closeOnError(err)
	}
	var greeting [2]byte
	if _, err = io.ReadFull(conn, greeting[:]); err != nil {
		return closeOnError(err)
	}
	if greeting[0] != 0x05 {
		return closeOnError(errors.New("proxy returned invalid socks version"))
	}
	if greeting[1] == 0xff {
		return closeOnError(errors.New("proxy rejected all authentication methods"))
	}
	if greeting[1] == 0x02 {
		if len(d.username) > 255 || len(d.password) > 255 {
			return closeOnError(errors.New("proxy credentials are too long"))
		}
		auth := []byte{0x01, byte(len(d.username))}
		auth = append(auth, d.username...)
		auth = append(auth, byte(len(d.password)))
		auth = append(auth, d.password...)
		if _, err = conn.Write(auth); err != nil {
			return closeOnError(err)
		}
		var ar [2]byte
		if _, err = io.ReadFull(conn, ar[:]); err != nil || ar[1] != 0x00 {
			if err == nil {
				err = errors.New("proxy username/password authentication failed")
			}
			return closeOnError(err)
		}
	} else if greeting[1] != 0x00 {
		return closeOnError(errors.New("proxy selected unsupported authentication"))
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return closeOnError(fmt.Errorf("invalid target address %q: %w", address, err))
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return closeOnError(fmt.Errorf("invalid target port: %w", err))
	}
	// socks5h always sends the hostname; the proxy performs DNS resolution.
	if len(host) > 255 {
		return closeOnError(errors.New("target hostname is too long"))
	}
	request := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	request = append(request, host...)
	var portBuf [2]byte
	binary.BigEndian.PutUint16(portBuf[:], uint16(port))
	request = append(request, portBuf[:]...)
	if _, err = conn.Write(request); err != nil {
		return closeOnError(err)
	}
	var head [4]byte
	if _, err = io.ReadFull(conn, head[:]); err != nil {
		return closeOnError(err)
	}
	if head[0] != 0x05 || head[1] != 0x00 {
		return closeOnError(fmt.Errorf("proxy connect failed (code %d)", head[1]))
	}
	addrLen := 0
	switch head[3] {
	case 0x01:
		addrLen = 4
	case 0x04:
		addrLen = 16
	case 0x03:
		var n [1]byte
		if _, err = io.ReadFull(conn, n[:]); err != nil {
			return closeOnError(err)
		}
		addrLen = int(n[0])
	default:
		return closeOnError(errors.New("proxy returned invalid bind address type"))
	}
	if _, err = io.CopyN(io.Discard, conn, int64(addrLen+2)); err != nil {
		return closeOnError(err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
