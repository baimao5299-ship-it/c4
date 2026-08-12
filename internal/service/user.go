// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

var (
	ErrInvalidCredentials = errors.New("service: invalid email or password")
	ErrSignupDisabled     = errors.New("service: signup disabled")
)

// RegisterUser 注册（注册即登录——handler 侧签发 JWT）：
// 快照读 settings.signup_enabled 开关（UpdateSetting 即时生效）→ email
// 唯一/格式 → 密码 ≤72 字节 → bcrypt DefaultCost(10)（sub2api 同参数）→
// 快照读 4 个新用户初始资源默认值 → CreateUser → temp_balance > 0 送临时
// 额度（插行失败不阻断注册，评审 M-2）。
func (s *Service) RegisterUser(ctx context.Context, email, password string) (*domain.User, error) {
	if s.settingValue("signup_enabled") != "true" {
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
	// 新用户初始资源：仅公开注册路径套默认；管理面 CreateUser 显式传值
	// （用户拍板，0 就是 0）。
	created, err := s.store.CreateUser(ctx, &domain.User{
		Email: email, PasswordHash: hash,
		Role: domain.RoleUser, Status: domain.UserStatusActive,
		MaxConcurrency: int(s.settingInt("default_user_max_concurrency")),
		Balance:        s.settingInt("default_user_balance"),
	})
	if err != nil {
		return nil, err
	}
	if temp := s.settingInt("default_user_temp_balance"); temp > 0 {
		expiresAt := time.Now().AddDate(0, 0, int(s.settingInt("default_user_temp_balance_ttl_days")))
		note := "signup bonus"
		if err := s.store.CreateTempBalance(ctx, created.ID, temp, &expiresAt, &note); err != nil {
			// 评审 M-2：赠品插行失败不阻断注册（否则注册报错 → 客户端重试
			// → 409 email 死锁）；仅告警，用户已创建成功。
			if s.log != nil {
				s.log.Warn("signup temp balance insert failed", logx.Int64("user_id", created.ID), logx.Error(err))
			}
		}
	}
	s.inv.Users()
	s.publish(ctx, notify.Change{Users: true})
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

// GetUser 用户详情（/admin/users/{id} 更新前置读取）。
func (s *Service) GetUser(ctx context.Context, id int64) (*domain.User, error) {
	u, err := s.store.GetUser(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return u, nil
}

// GetUserMe 当前用户信息（/user/auth/me）。
func (s *Service) GetUserMe(ctx context.Context, userID int64) (*domain.User, error) {
	return s.GetUser(ctx, userID)
}

// CreateUser 管理面创建用户（platform_admin 专属）：email 唯一/格式、密码
// ≤72 字节 → bcrypt（sub2api 同参数）→ role/status/max_concurrency/balance
// 落库 → invalidate（新用户入 Auth 状态快照）。价格倍率按组（T3.5 修正）经
// group_assignment 设置（SetGroupAssignments），用户本体无倍率字段。
func (s *Service) CreateUser(ctx context.Context, email, password string, role domain.Role, status domain.UserStatus, maxConcurrency int, balance int64) (*domain.User, error) {
	if !validEmail(email) {
		return nil, ErrInvalidInput
	}
	if password == "" || auth.ValidatePasswordLen(password) != nil {
		return nil, ErrInvalidInput
	}
	if !role.Valid() || !status.Valid() || maxConcurrency < 0 || balance < 0 {
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
		Role: role, Status: status,
		MaxConcurrency: maxConcurrency, Balance: balance,
	})
	if err != nil {
		return nil, err
	}
	s.inv.Users()
	s.publish(ctx, notify.Change{Users: true})
	if s.log != nil {
		s.log.Info("user created by admin", logx.Int64("id", created.ID), logx.String("email", email), logx.String("role", string(role)))
	}
	return created, nil
}

// ListUsers 用户列表（/admin/users；platform_admin 专属）。
func (s *Service) ListUsers(ctx context.Context, q repository.ListQuery) ([]*domain.User, int64, error) {
	if err := validateListQuery(q, listSortFields["users"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListUsers(ctx, q)
}

// UpdateUser 用户管理面更新（role/status/max_concurrency/balance）。用户状态/
// 并发/额度变更 → invalidate → Auth.Reload 全量刷新（评审 I-2）。价格倍率按
// 组（T3.5 修正）经 group_assignment 设置，用户本体无倍率字段。
func (s *Service) UpdateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	if !u.Role.Valid() || !u.Status.Valid() {
		return nil, ErrInvalidInput
	}
	if u.MaxConcurrency < 0 || u.Balance < 0 {
		return nil, ErrInvalidInput
	}
	updated, err := s.store.UpdateUser(ctx, u)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.inv.Users()
	s.publish(ctx, notify.Change{Users: true})
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
