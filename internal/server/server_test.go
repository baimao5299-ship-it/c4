package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHealthz(t *testing.T) {
	s := NewServer(Options{AdminToken: "tok", Logger: nil})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Contains(t, body, "inflight")
	require.Contains(t, body, "goroutines")
	require.Contains(t, body, "heap")
}

func TestUnknownPath404(t *testing.T) {
	s := NewServer(Options{AdminToken: "tok"})
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 404, rec.Code)
}

// 规格 §6.1：/admin/* 需要 Bearer admin token；无/错 token → 401。
func TestAdminAuthRequired(t *testing.T) {
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	s := NewServer(Options{AdminToken: "tok", AdminHandler: admin})

	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"no token", "", 401},
		{"wrong token", "Bearer nope", 401},
		{"non-bearer", "tok", 401},
		{"right token", "Bearer tok", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code)
		})
	}
}

// 规格 §10.6：全局在途上限，超限立即 429 + Retry-After: 1。
func TestInflightLimiterRejects(t *testing.T) {
	release := make(chan struct{})
	ai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(200)
	})
	s := NewServer(Options{AdminToken: "tok", MaxInflight: 1, AIHandler: ai})

	firstDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		firstDone <- rec.Code
	}()
	for s.inflight.Load() < 1 { // 等首请求进入 limiter
		runtime.Gosched()
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "1", rec.Header().Get("Retry-After"))

	close(release)
	require.Equal(t, http.StatusOK, <-firstDone)
	require.Zero(t, s.inflight.Load())
}
