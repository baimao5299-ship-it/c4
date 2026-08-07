package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/auth"
	"go-proxy-mini/internal/domain"
	userapi "go-proxy-mini/internal/handler/user"
	"go-proxy-mini/internal/service"
)

// fakeUserStatus 快照用户状态 provider（测试替身：直读 fake store 当前状态，
// 语义等同 Auth 快照在 invalidate 后反映 DB 状态）。
type fakeUserStatus struct{ store *fakeStore }

func (f fakeUserStatus) UserStatus(userID int64) (domain.UserStatus, bool) {
	u, err := f.store.GetUser(context.Background(), userID)
	if err != nil {
		return "", false
	}
	return u.Status, true
}

// newTestUserRouter /user 测试路由（真实 svc + fake store + 真实 Issuer）。
func newTestUserRouter(t *testing.T) (func(method, path, body, token string) *httptest.ResponseRecorder, *fakeStore, *auth.Issuer) {
	t.Helper()
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, func() {}, nil, &fakeKeys{}, nil)
	iss := auth.NewIssuer("test-secret")
	router := userapi.Router(svc, iss, fakeUserStatus{store: store})
	do := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	return do, store, iss
}

func TestUserRegisterLoginMe(t *testing.T) {
	do, _, _ := newTestUserRouter(t)

	// 注册成功（注册即登录：返回 JWT + 用户）
	rec := do(http.MethodPost, "/user/auth/register", `{"email":"new@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "register: %s", rec.Body.String())
	var resp userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token, "注册即登录")
	require.Equal(t, "new@example.com", *resp.User.Email)
	require.Equal(t, userapi.UserRole("user"), *resp.User.Role)

	// me（注册返回的 JWT 直接可用）
	rec = do(http.MethodGet, "/user/auth/me", "", resp.Token)
	require.Equal(t, http.StatusOK, rec.Code, "me: %s", rec.Body.String())
	var me userapi.User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	require.Equal(t, "new@example.com", *me.Email)

	// me 无 token → 401
	rec = do(http.MethodGet, "/user/auth/me", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 重复注册 → 409
	rec = do(http.MethodPost, "/user/auth/register", `{"email":"new@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusConflict, rec.Code, "dup register: %s", rec.Body.String())

	// 登录成功
	rec = do(http.MethodPost, "/user/auth/login", `{"email":"new@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "login: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token)

	// 口令错误 → 401
	rec = do(http.MethodPost, "/user/auth/login", `{"email":"new@example.com","password":"wrong"}`, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 邮箱格式非法 → 400
	rec = do(http.MethodPost, "/user/auth/register", `{"email":"not-an-email","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad email: %s", rec.Body.String())

	// 密码超长（>72 字节）→ 400
	longPass := strings.Repeat("a", 73)
	rec = do(http.MethodPost, "/user/auth/register", `{"email":"long@example.com","password":"`+longPass+`"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "long password: %s", rec.Body.String())
}

// 注册开关（settings.signup_enabled）关闭 → 403（DB 直读即时生效）。
func TestUserRegisterSignupDisabled(t *testing.T) {
	do, store, _ := newTestUserRouter(t)
	_, err := store.SetSetting(t.Context(), "signup_enabled", domain.SettingTypeSwitch, "false")
	require.NoError(t, err)
	rec := do(http.MethodPost, "/user/auth/register", `{"email":"x@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusForbidden, rec.Code, "signup disabled: %s", rec.Body.String())
}

// 禁用用户登录 → 401（与口令错误同文案，防枚举）。
func TestUserLoginDisabled(t *testing.T) {
	do, store, iss := newTestUserRouter(t)
	rec := do(http.MethodPost, "/user/auth/register", `{"email":"d@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, 200, rec.Code)
	u, err := store.GetUserByEmail(t.Context(), "d@example.com")
	require.NoError(t, err)
	u.Status = domain.UserStatusDisabled
	_, err = store.UpdateUser(t.Context(), u)
	require.NoError(t, err)
	rec = do(http.MethodPost, "/user/auth/login", `{"email":"d@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "disabled login: %s", rec.Body.String())

	// 禁用用户的既有 JWT → me 401（快照校验）
	token, err := iss.Issue(u.ID, u.Email, string(u.Role))
	require.NoError(t, err)
	rec = do(http.MethodGet, "/user/auth/me", "", token)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "disabled me: %s", rec.Body.String())
}

// --- 管理面 settings ---

func TestAdminSettings(t *testing.T) {
	_, _, do := newListTestRouter(t)

	// GET：默认值（signup_enabled=true）
	rec := do(http.MethodGet, "/admin/settings", "")
	require.Equal(t, http.StatusOK, rec.Code, "get: %s", rec.Body.String())
	var rows []Setting
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	require.Equal(t, "signup_enabled", *rows[0].Key)
	require.Equal(t, "true", *rows[0].Value)

	// PUT：switch 合法值
	rec = do(http.MethodPut, "/admin/settings", `{"key":"signup_enabled","value":"false"}`)
	require.Equal(t, http.StatusOK, rec.Code, "put: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Equal(t, "false", *rows[0].Value)

	// PUT：switch 非法值 → 400（类型化校验）
	rec = do(http.MethodPut, "/admin/settings", `{"key":"signup_enabled","value":"maybe"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad switch: %s", rec.Body.String())

	// PUT：未知 key → 400
	rec = do(http.MethodPut, "/admin/settings", `{"key":"unknown_key","value":"1"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "unknown key: %s", rec.Body.String())

	// PUT：number 类型校验（当前无 number 内置项——用非法场景兜底验证拒绝路径）
	rec = do(http.MethodPut, "/admin/settings", `{"key":"signup_enabled","value":"42"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
