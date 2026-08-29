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
