package user

import (
	"net/http"

	"go-proxy-mini/internal/auth"
	"go-proxy-mini/internal/domain"
)

// PostUserAuthRegister 注册（signup_enabled 开关检查在 service；注册即登录：
// 直接签发 JWT 返回，ServerInterface）。
func (h *UserAPI) PostUserAuthRegister(w http.ResponseWriter, r *http.Request) {
	var in UserAuthRegister
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	u, err := h.svc.RegisterUser(r.Context(), in.Email, in.Password)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	h.writeAuthResponse(w, u)
}

// PostUserAuthLogin 登录：bcrypt 校验 → JWT（ServerInterface）。
func (h *UserAPI) PostUserAuthLogin(w http.ResponseWriter, r *http.Request) {
	var in UserAuthLogin
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	u, err := h.svc.LoginUser(r.Context(), in.Email, in.Password)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	h.writeAuthResponse(w, u)
}

// GetUserAuthMe 当前用户信息（JWT 已由 Router 中间件验证，ServerInterface）。
func (h *UserAPI) GetUserAuthMe(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.GetUserMe(r.Context(), currentUserID(r))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIUser(u))
}

func (h *UserAPI) writeAuthResponse(w http.ResponseWriter, u *domain.User) {
	token, err := h.iss.Issue(u.ID, u.Email, string(u.Role))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "token issuance failed")
		return
	}
	writeJSON(w, http.StatusOK, UserAuthResponse{Token: token, User: toAPIUser(u)})
}

// currentUserID 取 RequireJWT 写入 context 的 claims.UserID（JWT 保护端点用）。
func currentUserID(r *http.Request) int64 {
	if claims, ok := auth.ClaimsFrom(r.Context()); ok {
		return claims.UserID
	}
	return 0
}
