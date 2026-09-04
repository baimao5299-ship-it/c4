// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"regexp"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/stretchr/testify/require"
)

type referralRegistrationStore struct {
	*fakeStore
	bindings map[int64]int64
}

func (f *referralRegistrationStore) GetUserByInviteCode(_ context.Context, code string) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.InviteCode == code {
			copy := *u
			return &copy, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (f *referralRegistrationStore) CreateUserWithReferral(ctx context.Context, u *domain.User, inviterID int64) (*domain.User, error) {
	created, err := f.CreateUser(ctx, u)
	if err != nil {
		return nil, err
	}
	f.bindings[created.ID] = inviterID
	return created, nil
}

func TestRandomLetters12(t *testing.T) {
	re := regexp.MustCompile(`^[A-Z]{12}$`)
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		code, err := randomLetters12()
		require.NoError(t, err)
		require.Regexp(t, re, code)
		seen[code] = struct{}{}
	}
	require.Len(t, seen, 100)
}

func TestRegisterUserWithInviteBindsExactlyAtCreation(t *testing.T) {
	base := newFakeStore()
	store := &referralRegistrationStore{fakeStore: base, bindings: make(map[int64]int64)}
	inviter, err := store.CreateUser(context.Background(), &domain.User{Email: "inviter@example.com", InviteCode: "ABCDEFGHIJKL", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)
	invitee, err := svc.RegisterUserWithInvite(context.Background(), "invitee@example.com", "s3cret-pass", "ABCDEFGHIJKL")
	require.NoError(t, err)
	require.Equal(t, inviter.ID, store.bindings[invitee.ID])
	require.Regexp(t, `^[A-Z]{12}$`, invitee.InviteCode)

	_, err = svc.RegisterUserWithInvite(context.Background(), "bad@example.com", "s3cret-pass", "NOT-A-CODE")
	require.ErrorIs(t, err, ErrInvalidInput)
}
