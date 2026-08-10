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

	"go-proxy-mini/internal/credential"
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/service"
)

type fakeSched struct{}

func (fakeSched) Runtime(id int64) (scheduler.RuntimeInfo, bool) {
	return scheduler.RuntimeInfo{Status: domain.StatusActive}, true
}

type fakeKeys struct{ upserted, deleted []string }

func (f *fakeKeys) Upsert(hash string, meta domain.KeyMeta) {
	f.upserted = append(f.upserted, hash)
}
func (f *fakeKeys) Delete(hash string) { f.deleted = append(f.deleted, hash) }

func newTestHandler(t *testing.T) *AdminAPI {
	t.Helper()
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
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
		"name":"openai-main","base_url":"https://api.openai.com",
		"supported_formats":["openai-chat","openai-responses"],"models":["gpt-4o","o3"],
		"format_models":{"openai-responses":["o3"]},
		"model_mapping":{"gpt-4o":"gpt-4o-2026-01-01"}}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	var tpl domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	require.True(t, tpl.FormatSupports(domain.FormatOpenAIResponses, "o3"), "format_models round-trip")
	require.False(t, tpl.FormatSupports(domain.FormatOpenAIResponses, "gpt-4o"), "responses 仅列表内模型")
	require.Equal(t, credential.TypeAPIKey, tpl.CredentialType, "缺省 credential_type → 响应含默认 api_key")

	// 非法 credential_type（号池生态类型未实现）→ 400
	recBad := do(http.MethodPost, "/admin/templates", `{
		"name":"bad","base_url":"https://u","supported_formats":["openai-chat"],
		"credential_type":"codex_oauth"}`)
	require.Equal(t, 400, recBad.Code, "非法 credential_type 必须 400: %s", recBad.Body.String())

	rec = do(http.MethodPost, "/admin/accounts", `{
		"name":"acc1","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk-x","weight":80,"max_concurrency":4}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
	var acc domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))

	rec = do(http.MethodPost, "/admin/groups", `{"name":"g1"}`)
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())
	var groupResp domain.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groupResp))
	require.Equal(t, "g1", groupResp.Name)
	require.Equal(t, domain.GroupVisibilityPublic, groupResp.Visibility, "缺省 visibility = public")

	// 账号侧绑定分组：PUT 账号 body 带 group_ids；回显经 GET /accounts/{id}/groups 核对。
	rec = do(http.MethodPut, "/admin/accounts/"+itoa(acc.ID),
		`{"name":"acc1","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk-x","group_ids":[`+itoa(groupResp.ID)+`]}`)
	require.Equal(t, 200, rec.Code, "account-side binding: %s", rec.Body.String())
	rec = do(http.MethodGet, "/admin/accounts/"+itoa(acc.ID)+"/groups", "")
	require.Equal(t, 200, rec.Code, "get account groups: %s", rec.Body.String())
	var accGroups AccountGroupsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &accGroups))
	require.Equal(t, []int64{groupResp.ID}, accGroups.GroupIds, "账号分组回显")

	// Phase 3a：rotate-key 端点已删除（key 轮换在用户面 /user/keys/{id}/rotate）→ 404
	rec = do(http.MethodPost, "/admin/groups/"+itoa(groupResp.ID)+"/rotate-key", "")
	require.Equal(t, 404, rec.Code, "rotate-key 端点已删除: %s", rec.Body.String())

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
		"name":"openai-main","base_url":"https://api.openai.com",
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
		"credential_type":"api_key",
		"supported_formats":["openai-chat","anthropic"],"models":["gpt-4o","o3"],
		"format_models":{"anthropic":["o3"]},
		"model_mapping":{"gpt-4o":"gpt-4o-2026-06-01"}}`)
	require.Equal(t, 200, rec.Code, "update: %s", rec.Body.String())
	var updated domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, "openai-main-v2", updated.Name)
	require.Equal(t, credential.TypeAPIKey, updated.CredentialType, "credential_type 全量更新透传")
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

// newListTestRouter 列表参数测试的接线：chi + admin token 中间件 + 挂载契约路由。
func newListTestRouter(t *testing.T) (*AdminAPI, http.Handler, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
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
	return h, r, do
}

// 列表响应从裸数组 → {total, rows} 的破坏性变更测试：全部参数绑定成功
// （fake store 不筛选，参数不报错 + 结构正确即通过）。
func TestGetTemplatesParams(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"openai-main","base_url":"https://api.openai.com",
		"supported_formats":["openai-chat"],"models":["gpt-4o"]}`)
	require.Equal(t, 200, rec.Code, "create: %s", rec.Body.String())

	rec = do(http.MethodGet, "/admin/templates?limit=5&offset=10&name=openai&sort=name&order=asc", "")
	require.Equal(t, 200, rec.Code, "list: %s", rec.Body.String())
	var body TemplateListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, int64(1), body.Total, "total")
	require.Len(t, body.Rows, 1, "rows")
	require.Equal(t, "openai-main", body.Rows[0].Name, "row name")
}

// status 多值（逗号分隔）+ template_id 筛选参数绑定；非法枚举值 → 400
// （openapi status 是纯 string 不校验枚举，handler 必须显式校验）。
func TestGetAccountsStatusMulti(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"openai-main","base_url":"https://api.openai.com",
		"supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	rec = do(http.MethodPost, "/admin/accounts", `{
		"name":"acc1","template_id":1,"upstream_key":"sk-x","status":"active"}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())

	rec = do(http.MethodGet, "/admin/accounts?status=active,disabled&template_id=1", "")
	require.Equal(t, 200, rec.Code, "list: %s", rec.Body.String())
	var body AccountListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, int64(1), body.Total, "total")
	require.Len(t, body.Rows, 1, "rows")

	// 非法 status 枚举 → 400（handoff 硬性要求：不校验会落 repo 裸 error → 500）
	rec = do(http.MethodGet, "/admin/accounts?status=bogus", "")
	require.Equal(t, 400, rec.Code, "invalid status: %s", rec.Body.String())
	var errBody map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Contains(t, errBody, "error", "must be ErrorResponse JSON")
}

// 非法 sort 值 → 400（service validateListQuery 白名单校验）。
func TestGetGroupsSortInvalid(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodGet, "/admin/groups?sort=bogus", "")
	require.Equal(t, 400, rec.Code, "invalid sort: %s", rec.Body.String())
	var errBody map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Contains(t, errBody, "error", "must be ErrorResponse JSON")
}

// TestTemplateGroupConflict409 重复 name 创建模板/组 → 409（此前裸透传 repo 唯一
// 约束错误 → 500），且响应消息含冲突详情（name 值）。
func TestTemplateGroupConflict409(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/admin/templates", `{
		"name":"dup","base_url":"https://api.example.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())

	rec = do(http.MethodPost, "/admin/templates", `{
		"name":"dup","base_url":"https://api2.example.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 409, rec.Code, "重复 name 创建模板必须 409: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), `name="dup"`, "409 消息含冲突详情")

	rec = do(http.MethodPost, "/admin/groups", `{"name":"dup-g"}`)
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())

	rec = do(http.MethodPost, "/admin/groups", `{"name":"dup-g"}`)
	require.Equal(t, 409, rec.Code, "重复 name 创建分组必须 409: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), `name="dup-g"`, "409 消息含冲突详情")
}

// errMsg 解析 {"error": ...} 响应体的 error 字段（引号经 JSON 转义，需解码后断言）。
func errMsg(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	s, ok := body["error"].(string)
	require.True(t, ok, "error must be string: %s", rec.Body.String())
	return s
}

// TestSingleResourceMissingID 单资源 GET/DELETE 缺 id → 404，且响应体消息
// 含缺失 id（与批量 404 同语义；Minor T5-2 清账：handler fake 的 Get/Delete
// 需返回带 id 错误，此前仅状态码断言/缺失）。
func TestSingleResourceMissingID(t *testing.T) {
	_, _, do := newListTestRouter(t)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/templates/999"},
		{http.MethodDelete, "/admin/templates/999"},
		{http.MethodGet, "/admin/accounts/999"},
		{http.MethodDelete, "/admin/accounts/999"},
		{http.MethodGet, "/admin/groups/999"},
		{http.MethodDelete, "/admin/groups/999"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(tc.method, tc.path, "")
			require.Equal(t, 404, rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
			var errBody map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
			errMsg, ok := errBody["error"].(string)
			require.True(t, ok, "error must be string: %s", rec.Body.String())
			require.Contains(t, errMsg, "id=999 missing", "404 消息含缺失 id: %s", rec.Body.String())
		})
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
