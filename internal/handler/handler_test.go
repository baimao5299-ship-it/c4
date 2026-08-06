package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/service"
)

type fakeSched struct{}

func (fakeSched) Runtime(id int64) (scheduler.RuntimeInfo, bool) {
	return scheduler.RuntimeInfo{Status: domain.StatusActive}, true
}

type fakeKeys struct{ upserted, deleted []string }

func (f *fakeKeys) Upsert(hash string, groupID int64) { f.upserted = append(f.upserted, hash) }
func (f *fakeKeys) Delete(hash string)                { f.deleted = append(f.deleted, hash) }

func newTestHandler(t *testing.T) *AdminAPI {
	t.Helper()
	store := newFakeStore()
	invalidate := func() {}
	svc := service.New(store, fakeSched{}, invalidate, &fakeKeys{}, nil)
	return New(svc)
}

func TestAdminFlow(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"openai-main","base_url":"https://api.openai.com/v1",
		"supported_formats":["openai-chat","openai-responses"],"models":["gpt-4o","o3"],
		"format_models":{"openai-responses":["o3"]},
		"model_mapping":{"gpt-4o":"gpt-4o-2026-01-01"}}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	var tpl domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	require.True(t, tpl.FormatSupports(domain.FormatOpenAIResponses, "o3"), "format_models round-trip")
	require.False(t, tpl.FormatSupports(domain.FormatOpenAIResponses, "gpt-4o"), "responses 仅列表内模型")

	rec = do(http.MethodPost, "/admin/accounts", `{
		"name":"acc1","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk-x","weight":80,"max_concurrency":4}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
	var acc domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))

	rec = do(http.MethodPost, "/admin/groups", `{"name":"g1"}`)
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())
	var groupResp struct {
		Group domain.Group `json:"group"`
		Key   string       `json:"key"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groupResp))
	require.True(t, strings.HasPrefix(groupResp.Key, "gk-"), "key=%s", groupResp.Key)

	rec = do(http.MethodPut, "/admin/groups/"+itoa(groupResp.Group.ID)+"/accounts", `{"account_ids":[`+itoa(acc.ID)+`]}`)
	require.Equal(t, 200, rec.Code, "set accounts: %s", rec.Body.String())

	rec = do(http.MethodPost, "/admin/groups/"+itoa(groupResp.Group.ID)+"/rotate-key", "")
	require.Equal(t, 200, rec.Code, "rotate: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"key":"gk-`)

	rec = do(http.MethodGet, "/admin/stats?granularity=day", "")
	require.Equal(t, 200, rec.Code, "stats: %s", rec.Body.String())

	// 未认证 → 401
	req := httptest.NewRequest(http.MethodGet, "/admin/templates", nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req)
	require.Equal(t, 401, rec2.Code)
}

func TestAdminUpdateTemplateRoundTrip(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"openai-main","base_url":"https://api.openai.com/v1",
		"supported_formats":["openai-chat","openai-responses"],"models":["gpt-4o","gpt-4o-mini","o3"],
		"format_models":{"openai-responses":["o3"]},
		"model_mapping":{"gpt-4o":"gpt-4o-2026-01-01"}}`)
	require.Equal(t, 200, rec.Code, "create: %s", rec.Body.String())
	var tpl domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))

	// PUT 全量 snake_case body：字段必须全部生效（评审发现：原实现直接解码
	// 无 tag 的 domain.Template，base_url/supported_formats/format_models/model_mapping
	// 被丢弃 → 校验失败 400）。
	rec = do(http.MethodPut, "/admin/templates/"+itoa(tpl.ID), `{
		"name":"openai-main-v2","base_url":"https://api.openai.com/v2",
		"supported_formats":["openai-chat","anthropic"],"models":["gpt-4o","o3"],
		"format_models":{"anthropic":["o3"]},
		"model_mapping":{"gpt-4o":"gpt-4o-2026-06-01"}}`)
	require.Equal(t, 200, rec.Code, "update: %s", rec.Body.String())
	var updated domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, "openai-main-v2", updated.Name)
	require.Equal(t, "https://api.openai.com/v2", updated.BaseURL, "base_url must round-trip")
	require.ElementsMatch(t, []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic}, updated.SupportedFormats, "supported_formats must round-trip")
	require.True(t, updated.FormatSupports(domain.FormatAnthropic, "o3"), "format_models must round-trip")
	require.False(t, updated.FormatSupports(domain.FormatAnthropic, "gpt-4o"), "format_models 限制生效")
	require.Equal(t, "gpt-4o-2026-06-01", updated.ModelMapping["gpt-4o"], "model_mapping must round-trip")

	// GET 确认已持久化
	rec = do(http.MethodGet, "/admin/templates/"+itoa(tpl.ID), "")
	require.Equal(t, 200, rec.Code, "get: %s", rec.Body.String())
	var got domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, updated.BaseURL, got.BaseURL, "update must persist")
}

// 评审：参数绑定失败（InvalidParamFormatError）必须输出契约 ErrorResponse
// JSON（{"error": ...}），而非生成的 http.Error 纯文本 400。
func TestParamBindErrorIsErrorResponse(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	do := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer admin-tok")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	for _, tc := range []struct {
		name, path string
	}{
		{"path param non-int", "/admin/templates/abc"},
		{"query limit non-int", "/admin/logs?limit=abc"},
		{"query date invalid", "/admin/logs?from=2026-13-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(tc.path)
			require.Equal(t, 400, rec.Code, "path %s: %s", tc.path, rec.Body.String())
			require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Contains(t, body, "error", "must be ErrorResponse JSON, got: %s", rec.Body.String())
		})
	}
}

// GetLogs 正常路径：limit/offset 缺省取契约默认值，返回 rows + total。
func TestGetLogs(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	req := httptest.NewRequest(http.MethodGet, "/admin/logs", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code, "logs: %s", rec.Body.String())
	var body LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Zero(t, body.Total)
	require.Empty(t, body.Rows)
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
