package service

import (
	"context"
	"errors"
	"net/mail"
	"strings"

	"go-proxy-mini/internal/auth"
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/pkg/logx"
)

var (
	ErrInvalidCredentials = errors.New("service: invalid email or password")
	ErrSignupDisabled     = errors.New("service: signup disabled")
)

// RegisterUser 注册（注册即登录——handler 侧签发 JWT）：
// settings.signup_enabled 开关（DB 直读，即时生效）→ email 唯一/格式 →
// 密码 ≤72 字节 → bcrypt DefaultCost(10)（sub2api 同参数）。
func (s *Service) RegisterUser(ctx context.Context, email, password string) (*domain.User, error) {
	setting, err := s.store.GetSetting(ctx, "signup_enabled")
	if err != nil {
		return nil, err
	}
	if setting.Value != "true" {
		return nil, ErrSignupDisabled
	}
	if !validEmail(email) {
		return nil, ErrInvalidInput
	}
	if password == "" || auth.ValidatePasswordLen(password) != nil {
		return nil, ErrInvalidInput
	}
	existing, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrConflict // email 唯一 → 409
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	created, err := s.store.CreateUser(ctx, &domain.User{
		Email: email, PasswordHash: hash,
		Role: domain.RoleUser, Status: domain.UserStatusActive,
	})
	if err != nil {
		return nil, err
	}
	s.invalidate()
	if s.log != nil {
		s.log.Info("user registered", logx.Int64("id", created.ID), logx.String("email", email))
	}
	return created, nil
}

// LoginUser 登录校验：email → bcrypt 校验 → 状态检查（禁用与口令错误同
// 文案 401，防枚举）。
func (s *Service) LoginUser(ctx context.Context, email, password string) (*domain.User, error) {
	u, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if u == nil || !auth.VerifyPassword(u.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}
	if u.Status != domain.UserStatusActive {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

// GetUserMe 当前用户信息（/user/auth/me）。
func (s *Service) GetUserMe(ctx context.Context, userID int64) (*domain.User, error) {
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return u, nil
}

// ListUsers 用户列表（/admin/users；platform_admin 专属）。
func (s *Service) ListUsers(ctx context.Context, q repository.ListQuery) ([]*domain.User, int64, error) {
	if err := validateListQuery(q, listSortFields["users"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListUsers(ctx, q)
}

// UpdateUser 用户管理面更新（role/status/max_concurrency/balance）。
// 用户状态/并发/额度变更 → invalidate → Auth.Reload 全量刷新（评审 I-2）。
func (s *Service) UpdateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	if !u.Role.Valid() || !u.Status.Valid() {
		return nil, ErrInvalidInput
	}
	if u.MaxConcurrency < 0 {
		return nil, ErrInvalidInput
	}
	updated, err := s.store.UpdateUser(ctx, u)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.invalidate()
	return updated, nil
}

// validEmail 简单邮箱格式校验（net/mail 解析 + 纯地址形式）。
func validEmail(email string) bool {
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return false
	}
	if strings.Contains(email, "..") {
		return false
	}
	return true
}
