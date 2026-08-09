package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/auth"
	"go-proxy-mini/internal/domain"
	userapi "go-proxy-mini/internal/handler/user"
	"go-proxy-mini/internal/service"
)

// newSharedRouters 共享 fakeStore 的 admin + user 双路由（Task 4 端到端：
// 管理面建组/授予 → 用户面选组建 key），返回 doAdmin/doUser。
func newSharedRouters(t *testing.T) (doAdmin, doUser func(method, path, body, token string) *httptest.ResponseRecorder, store *fakeStore) {
	t.Helper()
	store = newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, &fakeKeys{}, nil)

	// admin 路由（静态 token 中间件，模拟 server 层 /admin 鉴权）
	adminH := New(svc)
	ar := chi.NewRouter()
	ar.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	ar.Mount("/", adminH.Router())

	// user 路由（真实 Router：公开/RequireJWT 分流）
	iss := auth.NewIssuer("test-secret")
	ur := userapi.Router(svc, iss, fakeUserStatus{store: store})

	doAdmin = func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.Header.Set("Authorization", "Bearer admin-tok")
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ar.ServeHTTP(rec, req)
		return rec
	}
	doUser = func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ur.ServeHTTP(rec, req)
		return rec
	}
	return doAdmin, doUser, store
}

// registerAndGet 注册并返回 JWT + 用户（handler 测试 helper）。
func registerAndGet(t *testing.T, doUser func(method, path, body, token string) *httptest.ResponseRecorder, email string) (string, int64) {
	t.Helper()
	rec := doUser(http.MethodPost, "/user/auth/register",
		`{"email":"`+email+`","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "register: %s", rec.Body.String())
	var resp userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Token, *resp.User.ID
}

// TestUserKeysLifecycle 用户面 key 全生命周期 + 组可选性校验：
// public 可建 → private 未授予 400 → 管理面授予后可建 → 更新/轮换/删除 →
// 他人 key 不可见（404）。
func TestUserKeysLifecycle(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	// 管理面建组：public + private
	rec := doAdmin(http.MethodPost, "/admin/groups", `{"name":"public-g"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create public group: %s", rec.Body.String())
	var pubG Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pubG))
	require.Equal(t, GroupVisibility("public"), *pubG.Visibility, "缺省 visibility = public")

	rec = doAdmin(http.MethodPost, "/admin/groups", `{"name":"private-g","visibility":"private"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create private group: %s", rec.Body.String())
	var privG Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &privG))
	require.Equal(t, GroupVisibility("private"), *privG.Visibility)

	token, userID := registerAndGet(t, doUser, "alice@example.com")

	// /user/groups：只看到 public（private 未授予）
	rec = doUser(http.MethodGet, "/user/groups", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "groups: %s", rec.Body.String())
	var groups []Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	require.Len(t, groups, 1, "private 未授予不可见: %s", rec.Body.String())
	require.Equal(t, *pubG.ID, *groups[0].ID)

	// public 组建 key → 200（raw 明文仅本次返回）
	rec = doUser(http.MethodPost, "/user/keys", `{"name":"k1","group_id":`+itoa(*pubG.ID)+`}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "create key public: %s", rec.Body.String())
	var created userapi.KeyWithSecret
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.Key, "raw 明文必须返回")
	require.True(t, strings.HasPrefix(created.Key, "gk-"), "key 前缀约定")
	require.NotNil(t, created.KeyPrefix)
	require.Equal(t, int64(0), *created.Quota, "缺省 quota = 0 不限")

	// private 组未授予 → 400（组可选性校验）
	rec = doUser(http.MethodPost, "/user/keys", `{"name":"k2","group_id":`+itoa(*privG.ID)+`}`, token)
	require.Equal(t, http.StatusBadRequest, rec.Code, "private not granted: %s", rec.Body.String())

	// 管理面授予 → 可建
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*privG.ID)+"/assignments",
		`{"user_ids":[`+itoa(userID)+`]}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "assign: %s", rec.Body.String())
	var assigned GroupAssignmentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &assigned))
	require.Equal(t, []int64{userID}, assigned.UserIds)

	rec = doUser(http.MethodPost, "/user/keys", `{"name":"k2","group_id":`+itoa(*privG.ID)+`}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "create key granted private: %s", rec.Body.String())
	var k2 userapi.KeyWithSecret
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &k2))

	// 列表：2 个
	rec = doUser(http.MethodGet, "/user/keys", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "list keys: %s", rec.Body.String())
	var list userapi.KeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)

	// 更新：max_concurrency/status
	rec = doUser(http.MethodPut, "/user/keys/"+itoa(*created.ID),
		`{"max_concurrency":5,"status":"disabled"}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "update key: %s", rec.Body.String())
	var updated userapi.Key
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, 5, *updated.MaxConcurrency)
	require.Equal(t, userapi.KeyStatus("disabled"), *updated.Status)

	// 轮换：新明文 ≠ 旧明文；prefix 变化
	rec = doUser(http.MethodPost, "/user/keys/"+itoa(*created.ID)+"/rotate", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "rotate key: %s", rec.Body.String())
	var rotated userapi.KeyWithSecret
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rotated))
	require.NotEqual(t, created.Key, rotated.Key, "轮换后明文必须变化")
	require.NotEqual(t, created.KeyPrefix, rotated.KeyPrefix, "轮换后 prefix 必须变化")

	// 删除 → 列表剩 1
	rec = doUser(http.MethodDelete, "/user/keys/"+itoa(*created.ID), "", token)
	require.Equal(t, http.StatusOK, rec.Code, "delete key: %s", rec.Body.String())
	rec = doUser(http.MethodGet, "/user/keys", "", token)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)

	// 他人 key 不可见：bob 读 alice 的 key → 404（防越权探测）
	bobToken, _ := registerAndGet(t, doUser, "bob@example.com")
	rec = doUser(http.MethodGet, "/user/keys/"+itoa(*k2.ID), "", bobToken)
	require.Equal(t, http.StatusNotFound, rec.Code, "cross-user key: %s", rec.Body.String())
	rec = doUser(http.MethodDelete, "/user/keys/"+itoa(*k2.ID), "", bobToken)
	require.Equal(t, http.StatusNotFound, rec.Code, "cross-user delete: %s", rec.Body.String())

	// 不存在 key → 404
	rec = doUser(http.MethodGet, "/user/keys/99999", "", token)
	require.Equal(t, http.StatusNotFound, rec.Code, "missing key: %s", rec.Body.String())

	// 未认证访问业务端点 → 401（RequireJWT）
	rec = doUser(http.MethodGet, "/user/keys", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "no token: %s", rec.Body.String())
	rec = doUser(http.MethodGet, "/user/groups", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestUserLogsOwnOnly /user/logs 强制 user_id = 当前用户（越权过滤在
// service/repo 层；fake store 按 user_id 过滤——测试验证隔离语义）。
func TestUserLogsOwnOnly(t *testing.T) {
	_, doUser, store := newSharedRouters(t)
	tokenA, userA := registerAndGet(t, doUser, "a@example.com")
	_, userB := registerAndGet(t, doUser, "b@example.com")

	// 直接向 store 灌入两个用户的日志
	store.mu.Lock()
	store.logs = []*domain.UsageLog{
		{ID: 1, UserID: userA, RequestID: "r-a1", Model: "gpt-4o", Format: domain.FormatOpenAIChat},
		{ID: 2, UserID: userB, RequestID: "r-b1", Model: "gpt-4o", Format: domain.FormatOpenAIChat},
		{ID: 3, UserID: userA, RequestID: "r-a2", Model: "o3", Format: domain.FormatOpenAIResponses},
	}
	store.mu.Unlock()

	rec := doUser(http.MethodGet, "/user/logs", "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "user logs: %s", rec.Body.String())
	var body userapi.LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, int64(2), body.Total, "只看到自己的日志: %s", rec.Body.String())
	for _, r := range body.Rows {
		require.Equal(t, userA, *r.UserID, "日志必须归属当前用户")
	}
}

// TestAdminUsers 管理面用户 CRUD：创建（email 唯一/格式/密码长度）→
// 列表 → 更新（role/status/max_concurrency）→ 变更经 fakeUserStatus
// 快照即时可见。
func TestAdminUsers(t *testing.T) {
	doAdmin, _, _ := newSharedRouters(t)

	// 创建用户（缺省 role=user/status=active）
	rec := doAdmin(http.MethodPost, "/admin/users",
		`{"email":"carol@example.com","password":"s3cret-pass","max_concurrency":3}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create user: %s", rec.Body.String())
	var created User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, "carol@example.com", *created.Email)
	require.Equal(t, UserRole("user"), *created.Role)
	require.Equal(t, UserStatus("active"), *created.Status)
	require.Equal(t, 3, *created.MaxConcurrency)
	require.NotContains(t, rec.Body.String(), "password", "口令散列永不下发")

	// email 重复 → 409
	rec = doAdmin(http.MethodPost, "/admin/users",
		`{"email":"carol@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusConflict, rec.Code, "dup email: %s", rec.Body.String())

	// 邮箱格式非法 → 400
	rec = doAdmin(http.MethodPost, "/admin/users",
		`{"email":"not-an-email","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad email: %s", rec.Body.String())

	// 密码超长（>72 字节）→ 400
	rec = doAdmin(http.MethodPost, "/admin/users",
		`{"email":"long@example.com","password":"`+strings.Repeat("a", 73)+`"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "long password: %s", rec.Body.String())

	// 列表 + email 筛选
	rec = doAdmin(http.MethodGet, "/admin/users?email=carol", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "list users: %s", rec.Body.String())
	var list UserListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)

	// 更新：升级 platform_admin + 禁用
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID),
		`{"role":"platform_admin","status":"disabled","max_concurrency":0}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "update user: %s", rec.Body.String())
	var updated User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, UserRole("platform_admin"), *updated.Role)
	require.Equal(t, UserStatus("disabled"), *updated.Status)

	// 非法 role → 400
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID), `{"role":"bogus"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad role: %s", rec.Body.String())

	// 缺失用户 → 404
	rec = doAdmin(http.MethodPut, "/admin/users/99999", `{"role":"user"}`, "")
	require.Equal(t, http.StatusNotFound, rec.Code, "missing user: %s", rec.Body.String())
}

// TestAdminGroupAssignments 替换语义：授予/撤销/清空；用户缺失 → 404；
// 重复/非法 id → 400。
func TestAdminGroupAssignments(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	rec := doAdmin(http.MethodPost, "/admin/groups", `{"name":"g","visibility":"private"}`, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var g Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))

	token1, uid1 := registerAndGet(t, doUser, "u1@example.com")
	token2, uid2 := registerAndGet(t, doUser, "u2@example.com")

	// 授予两人
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments",
		`{"user_ids":[`+itoa(uid1)+`,`+itoa(uid2)+`]}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "assign two: %s", rec.Body.String())
	var resp GroupAssignmentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.ElementsMatch(t, []int64{uid1, uid2}, resp.UserIds)

	// 两人都可见 private 组
	for _, token := range []string{token1, token2} {
		rec = doUser(http.MethodGet, "/user/groups", "", token)
		var groups []Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
		require.Len(t, groups, 1, "授予后可见: %s", rec.Body.String())
	}

	// 替换语义：只留 uid1 → uid2 被撤销
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments",
		`{"user_ids":[`+itoa(uid1)+`]}`, "")
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doUser(http.MethodGet, "/user/groups", "", token2)
	var groups2 []Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups2))
	require.Empty(t, groups2, "撤销后不可见: %s", rec.Body.String())

	// 清空（幂等重放授予）
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", `{"user_ids":[]}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "clear: %s", rec.Body.String())
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", `{"user_ids":[]}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "clear idempotent")

	// 用户缺失 → 404；组缺失 → 404
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", `{"user_ids":[99999]}`, "")
	require.Equal(t, http.StatusNotFound, rec.Code, "missing user: %s", rec.Body.String())
	rec = doAdmin(http.MethodPut, "/admin/groups/99999/assignments", `{"user_ids":[`+itoa(uid1)+`]}`, "")
	require.Equal(t, http.StatusNotFound, rec.Code, "missing group: %s", rec.Body.String())

	// 重复 user_id → 400
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments",
		`{"user_ids":[`+itoa(uid1)+`,`+itoa(uid1)+`]}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "dup user: %s", rec.Body.String())
}

// TestAdminLogsUserFilter /admin/logs user_id 参数绑定（admin 看全部——
// 不传 user_id 不过滤；传了按用户过滤）。
func TestAdminLogsUserFilter(t *testing.T) {
	doAdmin, _, store := newSharedRouters(t)

	store.mu.Lock()
	store.logs = []*domain.UsageLog{
		{ID: 1, UserID: 7, RequestID: "r7", Model: "gpt-4o", Format: domain.FormatOpenAIChat},
		{ID: 2, UserID: 8, RequestID: "r8", Model: "o3", Format: domain.FormatOpenAIResponses},
	}
	store.mu.Unlock()

	// 不传 user_id → 全部
	rec := doAdmin(http.MethodGet, "/admin/logs", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var body LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, int64(2), body.Total)

	// user_id=7 → 1 条
	rec = doAdmin(http.MethodGet, "/admin/logs?user_id=7", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "user filter: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, int64(1), body.Total)
	require.Equal(t, int64(7), *body.Rows[0].UserID)
}
