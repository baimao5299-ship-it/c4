// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
)

// fakeOpsWorker 测试替身（StatsProvider 契约）。
type fakeOpsWorker struct {
	name  string
	stats any
}

func (f fakeOpsWorker) Name() string { return f.name }
func (f fakeOpsWorker) Stats() any   { return f.stats }

// typedStats 响应 typed struct 断言用本地类型（模拟各 worker 模块的 Stats 返回）。
type typedStats struct {
	Pending int64 `json:"pending"`
}

// TestOpsWorkersAdminAuth /ops/workers 与 /admin 同鉴权：静态 admin token /
// platform_admin JWT 可达；无/错 token、user JWT → 401。
func TestOpsWorkersAdminAuth(t *testing.T) {
	iss := auth.NewIssuer("secret")
	adminTok, err := iss.Issue(1, "admin@example.com", string(domain.RolePlatformAdmin))
	require.NoError(t, err)
	userTok, err := iss.Issue(2, "user@example.com", string(domain.RoleUser))
	require.NoError(t, err)
	s := NewServer(Options{
		AdminToken: "tok",
		JWTIssuer:  iss,
		UserStatus: fakeUserStatus{},
		OpsWorkers: []StatsProvider{fakeOpsWorker{"billing", typedStats{Pending: 3}}},
	})

	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"no token", "", 401},
		{"wrong token", "Bearer nope", 401},
		{"non-bearer", "tok", 401},
		{"user JWT", "Bearer " + userTok, 401},
		{"static token", "Bearer tok", 200},
		{"platform_admin JWT", "Bearer " + adminTok, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ops/workers", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code, tc.name)
		})
	}
}

// TestOpsWorkersResponse 响应 typed struct（非 map[string]any）：workers 条目
// 名称 + stats 透传、snapshots 区、generated_at。
func TestOpsWorkersResponse(t *testing.T) {
	s := NewServer(Options{
		AdminToken: "tok",
		OpsWorkers: []StatsProvider{
			fakeOpsWorker{"billing", typedStats{Pending: 3}},
			fakeOpsWorker{"retention", typedStats{Pending: 0}},
		},
		OpsSnapshots: func() []SnapshotState {
			return []SnapshotState{{Name: "auth", Scopes: []string{"settings"}, LastReload: time.Now().UTC()}}
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/ops/workers", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)

	var resp WorkersResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Workers, 2)
	require.Equal(t, "billing", resp.Workers[0].Name)
	// stats 为 typed struct 的 JSON 透传（模块级测试断言具体类型，此处断言字段）。
	st, ok := resp.Workers[0].Stats.(map[string]any)
	require.True(t, ok, "stats 应为对象")
	require.Equal(t, float64(3), st["pending"])
	require.Len(t, resp.Snapshots, 1)
	require.Equal(t, "auth", resp.Snapshots[0].Name)
	require.False(t, resp.GeneratedAt.IsZero(), "generated_at 非零")
}

// TestOpsWorkersSPAFallback SPA fallback 不吞 /ops/workers：WebFS 在场时无
// token 的 /ops/workers 仍 401（JSON，非 index.html）；未知路径照旧回 index.html。
func TestOpsWorkersSPAFallback(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte(`<html>spa</html>`)},
		"assets/app.js": &fstest.MapFile{Data: []byte(`console.log("x")`)},
	}
	s := NewServer(Options{
		AdminToken: "tok",
		WebFS:      fsys,
		OpsWorkers: []StatsProvider{fakeOpsWorker{"billing", typedStats{}}},
	})

	req := httptest.NewRequest(http.MethodGet, "/ops/workers", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 401, rec.Code, "/ops/workers 不得落入 SPA fallback 回 index.html")
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.NotContains(t, rec.Body.String(), "<html>spa")

	// 对照组：真正未知路径仍回 index.html（fallback 未破坏）。
	req = httptest.NewRequest(http.MethodGet, "/groups", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
}

// TestOpsWorkersNotRegistered 未装配 OpsWorkers → 路由不挂（404 非 fallback）。
func TestOpsWorkersNotRegistered(t *testing.T) {
	fsys := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(`<html>spa</html>`)}}
	s := NewServer(Options{AdminToken: "tok", WebFS: fsys})
	req := httptest.NewRequest(http.MethodGet, "/ops/workers", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 404, rec.Code, "未装配 OpsWorkers 时 /ops/workers 不挂路由")
	require.NotContains(t, rec.Body.String(), "<html>spa", "白名单 404，不回 index.html")
}
