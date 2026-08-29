// SPDX-License-Identifier: AGPL-3.0-or-later

package user

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAuthRateLimiterSeparatesEndpointsAndSources(t *testing.T) {
	var calls int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	})
	h := withAuthRateLimit(next, AuthRateLimits{LoginPerMinute: 2, RegisterPerMinute: 1, CodePerMinute: 1, ResetPerMinute: 1})

	request := func(path, remote string) int {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := request("/api/user/auth/login", "192.0.2.1:1000"); got != http.StatusNoContent {
		t.Fatalf("first login status = %d", got)
	}
	if got := request("/api/user/auth/login", "192.0.2.1:1000"); got != http.StatusNoContent {
		t.Fatalf("second login status = %d", got)
	}
	if got := request("/api/user/auth/login", "192.0.2.1:1000"); got != http.StatusTooManyRequests {
		t.Fatalf("third login status = %d, want 429", got)
	}
	if got := request("/api/user/auth/login", "192.0.2.2:1000"); got != http.StatusNoContent {
		t.Fatalf("different source login status = %d", got)
	}
	if got := request("/api/user/auth/register", "192.0.2.1:1000"); got != http.StatusNoContent {
		t.Fatalf("separate endpoint status = %d", got)
	}
	if calls != 4 {
		t.Fatalf("next handler calls = %d, want 4", calls)
	}
}

func TestAuthRateLimiterExpiresWindow(t *testing.T) {
	l := newAuthRateLimiter()
	now := time.Unix(100, 0)
	l.now = func() time.Time { return now }
	if ok, _ := l.allow("k", 1); !ok {
		t.Fatal("first request rejected")
	}
	if ok, _ := l.allow("k", 1); ok {
		t.Fatal("second request accepted inside window")
	}
	now = now.Add(time.Minute)
	if ok, _ := l.allow("k", 1); !ok {
		t.Fatal("request rejected after window expiry")
	}
}

func TestAuthSourceKeyUsesForwardedIPOnlyWhenConfigured(t *testing.T) {
	proxyReq := httptest.NewRequest(http.MethodPost, "/api/user/auth/login", nil)
	// A host-side reverse proxy reaches a Docker container through the bridge
	// gateway, so RemoteAddr is not necessarily loopback.
	proxyReq.RemoteAddr = "172.17.0.1:443"
	proxyReq.Header.Set("X-Forwarded-For", "198.51.100.7, 127.0.0.1")
	if got := authSourceKey(proxyReq, true); got != "198.51.100.7" {
		t.Fatalf("trusted proxy source = %q, want forwarded client IP", got)
	}
	if got := authSourceKey(proxyReq, false); got != "172.17.0.1" {
		t.Fatalf("untrusted proxy source = %q, want remote peer", got)
	}

	directReq := httptest.NewRequest(http.MethodPost, "/api/user/auth/login", nil)
	directReq.RemoteAddr = "203.0.113.9:443"
	directReq.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := authSourceKey(directReq, false); got != "203.0.113.9" {
		t.Fatalf("direct source = %q, want remote peer", got)
	}
}

func TestAuthSourceKeyRejectsInvalidForwardedIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/user/auth/login", nil)
	req.RemoteAddr = "[::1]:443"
	req.Header.Set("X-Forwarded-For", "not-an-ip")
	req.Header.Set("X-Real-IP", "2001:db8::8")
	if got := authSourceKey(req, true); got != "2001:db8::8" {
		t.Fatalf("fallback source = %q, want validated X-Real-IP", got)
	}
}

func TestAuthSourceKeyMatchesCDNHeaderOrder(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/user/auth/login", nil)
	req.RemoteAddr = "172.17.0.1:443"
	req.Header.Set("CF-Connecting-IP", "198.51.100.42")
	req.Header.Set("True-Client-IP", "198.51.100.43")
	req.Header.Set("X-Forwarded-For", "198.51.100.44, 172.17.0.1")
	req.Header.Set("X-Real-IP", "198.51.100.45")
	if got := authSourceKey(req, true); got != "198.51.100.42" {
		t.Fatalf("CDN source = %q, want CF-Connecting-IP", got)
	}
	req.Header.Set("CF-Connecting-IP", "not-an-ip")
	if got := authSourceKey(req, true); got != "198.51.100.43" {
		t.Fatalf("fallback CDN source = %q, want True-Client-IP", got)
	}
}
