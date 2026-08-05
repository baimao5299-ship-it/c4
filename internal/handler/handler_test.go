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

func newTestHandler(t *testing.T) *Handler {
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
	h.Routes(r)

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
		"default_format":"openai-chat","models":["gpt-4o"],
		"model_formats":{"o3":"openai-responses"},
		"model_mapping":{"gpt-4o":"gpt-4o-2026-01-01"}}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	var tpl domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	require.Equal(t, domain.FormatOpenAIResponses, tpl.FormatFor("o3"), "format override")

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

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
