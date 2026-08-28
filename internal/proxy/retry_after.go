// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// responseHeadersError carries response metadata across the existing
// UpstreamCaller error boundary without changing its public method signature.
// The body and status continue to travel through the normal return values.
type responseHeadersError struct {
	header http.Header
	err    error
}

func (e *responseHeadersError) Error() string {
	if e == nil || e.err == nil {
		return "upstream response metadata"
	}
	return e.err.Error()
}

func (e *responseHeadersError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *responseHeadersError) ResponseHeaders() http.Header {
	if e == nil {
		return nil
	}
	return cloneResponseHeaders(e.header)
}

func responseHeadersFromError(err error) http.Header {
	for err != nil {
		var provider interface{ ResponseHeaders() http.Header }
		if errors.As(err, &provider) {
			if h := provider.ResponseHeaders(); h != nil {
				return h
			}
		}
		// openai-go and anthropic-sdk-go expose *http.Response on their API
		// error structs but keep the concrete error type in a package-specific
		// internal path. Reflection is limited to this error path.
		v := reflect.ValueOf(err)
		for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
			if v.IsNil() {
				break
			}
			v = v.Elem()
		}
		if v.IsValid() && v.Kind() == reflect.Struct {
			f := v.FieldByName("Response")
			if f.IsValid() && f.Type() == reflect.TypeOf((*http.Response)(nil)) && f.CanInterface() {
				if resp, ok := f.Interface().(*http.Response); ok && resp != nil {
					return cloneResponseHeaders(resp.Header)
				}
			}
		}
		err = errors.Unwrap(err)
	}
	return nil
}

func cloneResponseHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header, len(src))
	for k, values := range src {
		dst[k] = append([]string(nil), values...)
	}
	return dst
}

// retryAfterDeadline parses standard Retry-After plus the common millisecond
// extension. Invalid, past, or zero values return nil so the scheduler can use
// its bounded per-account exponential fallback.
func retryAfterDeadline(now time.Time, hdr http.Header, err error) *time.Time {
	if hdr == nil {
		hdr = responseHeadersFromError(err)
	}
	if hdr == nil {
		return nil
	}
	if raw := strings.TrimSpace(hdr.Get("Retry-After")); raw != "" {
		if seconds, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && seconds >= 0 {
			d := time.Duration(seconds) * time.Second
			if d < time.Second {
				d = time.Second
			}
			v := now.Add(d)
			return &v
		}
		if at, parseErr := http.ParseTime(raw); parseErr == nil && at.After(now) {
			return &at
		}
	}
	if raw := strings.TrimSpace(hdr.Get("X-Retry-After")); raw != "" {
		if seconds, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && seconds >= 0 {
			d := time.Duration(seconds) * time.Second
			if d < time.Second {
				d = time.Second
			}
			v := now.Add(d)
			return &v
		}
	}
	if raw := strings.TrimSpace(hdr.Get("Retry-After-Ms")); raw != "" {
		if millis, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && millis >= 0 {
			d := time.Duration(millis) * time.Millisecond
			if d < time.Second {
				d = time.Second
			}
			v := now.Add(d)
			return &v
		}
	}
	return nil
}
