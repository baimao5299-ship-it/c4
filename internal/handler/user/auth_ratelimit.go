// SPDX-License-Identifier: AGPL-3.0-or-later

package user

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AuthRateLimits controls the per-process guard on public authentication
// endpoints. The limiter is deliberately local: it protects bcrypt and mail
// resources immediately on every instance while Redis remains the source of
// truth for durable auth state.
type AuthRateLimits struct {
	LoginPerMinute    int
	RegisterPerMinute int
	CodePerMinute     int
	ResetPerMinute    int
	// TrustForwardedIP is valid only when C4 is reachable exclusively through a
	// reverse proxy that overwrites the forwarding headers. It is deliberately
	// shared with proxy.behind_cdn at the composition root so authentication
	// limits and request audit logs use the same client identity policy.
	TrustForwardedIP bool
}

func (l AuthRateLimits) withDefaults() AuthRateLimits {
	if l.LoginPerMinute <= 0 {
		l.LoginPerMinute = 20
	}
	if l.RegisterPerMinute <= 0 {
		l.RegisterPerMinute = 5
	}
	if l.CodePerMinute <= 0 {
		l.CodePerMinute = 3
	}
	if l.ResetPerMinute <= 0 {
		l.ResetPerMinute = 5
	}
	return l
}

type authRateLimiter struct {
	mu      sync.Mutex
	entries map[string]authRateEntry
	window  time.Duration
	maxKeys int
	now     func() time.Time
}

type authRateEntry struct {
	started time.Time
	count   int
}

func newAuthRateLimiter() *authRateLimiter {
	return &authRateLimiter{
		entries: make(map[string]authRateEntry),
		window:  time.Minute,
		maxKeys: 10000,
		now:     time.Now,
	}
}

func (l *authRateLimiter) allow(key string, limit int) (bool, time.Duration) {
	if limit < 1 {
		return false, l.window
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok || now.Sub(entry.started) >= l.window {
		if len(l.entries) >= l.maxKeys {
			l.evictExpired(now)
			if len(l.entries) >= l.maxKeys {
				// Bounded memory is more important than preserving an old
				// counter when an attacker rotates source addresses.
				for k := range l.entries {
					delete(l.entries, k)
					break
				}
			}
		}
		l.entries[key] = authRateEntry{started: now, count: 1}
		return true, 0
	}
	if entry.count >= limit {
		return false, time.Until(entry.started.Add(l.window))
	}
	entry.count++
	l.entries[key] = entry
	return true, 0
}

func (l *authRateLimiter) evictExpired(now time.Time) {
	for key, entry := range l.entries {
		if now.Sub(entry.started) >= l.window {
			delete(l.entries, key)
		}
	}
}

func authSourceKey(r *http.Request, trustForwarded bool) string {
	if r == nil {
		return ""
	}
	if trustForwarded {
		if forwarded := forwardedClientIP(r); forwarded != "" {
			return forwarded
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	// httptest and custom transports may supply a bare address. Forwarded
	// headers are still ignored unless the deployment explicitly enabled them.
	return strings.TrimSpace(r.RemoteAddr)
}

func forwardedClientIP(r *http.Request) string {
	// Keep the auth limiter's trusted-proxy order aligned with the request
	// audit path. These headers are trusted only when the composition root has
	// explicitly enabled TrustForwardedIP and the public proxy overwrites them.
	for _, raw := range []string{
		r.Header.Get("CF-Connecting-IP"),
		r.Header.Get("True-Client-IP"),
		r.Header.Get("X-Forwarded-For"),
		r.Header.Get("X-Real-IP"),
	} {
		for _, part := range strings.Split(raw, ",") {
			candidate := strings.TrimSpace(part)
			if ip := net.ParseIP(candidate); ip != nil {
				return ip.String()
			}
		}
	}
	return ""
}

func authRateLimitForPath(path string, limits AuthRateLimits) int {
	switch path {
	case "/api/user/auth/login":
		return limits.LoginPerMinute
	case "/api/user/auth/register":
		return limits.RegisterPerMinute
	case "/api/user/auth/register-code", "/api/user/auth/forgot-password":
		return limits.CodePerMinute
	case "/api/user/auth/reset-password":
		return limits.ResetPerMinute
	default:
		return 0
	}
}

func withAuthRateLimit(next http.Handler, limits AuthRateLimits) http.Handler {
	if limits.LoginPerMinute == 0 && limits.RegisterPerMinute == 0 && limits.CodePerMinute == 0 && limits.ResetPerMinute == 0 {
		// Keep the historical Router(svc, ...) construction behavior for
		// embedded integrations and focused tests. The production composition
		// root always passes explicit non-zero defaults from config.
		return next
	}
	limits = limits.withDefaults()
	limiter := newAuthRateLimiter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := authRateLimitForPath(r.URL.Path, limits)
		if limit == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if ok, retryAfter := limiter.allow(r.URL.Path+"\x00"+authSourceKey(r, limits.TrustForwardedIP), limit); !ok {
			seconds := int((retryAfter + time.Second - 1) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
