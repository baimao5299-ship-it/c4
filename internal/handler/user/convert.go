package user

import (
	"go-proxy-mini/internal/domain"
)

// toAPIUser 用户领域对象 → 契约类型（口令散列永不下发）。
func toAPIUser(u *domain.User) User {
	r := UserRole(u.Role)
	st := UserStatus(u.Status)
	return User{
		ID:             &u.ID,
		Email:          &u.Email,
		Role:           &r,
		Status:         &st,
		MaxConcurrency: &u.MaxConcurrency,
		Balance:        &u.Balance,
		CreatedAt:      &u.CreatedAt,
		UpdatedAt:      &u.UpdatedAt,
	}
}
