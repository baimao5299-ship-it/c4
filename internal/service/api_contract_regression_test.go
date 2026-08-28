// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Management requests use enum values at the API boundary. An unknown value
// must not silently become a public group because that changes visibility and
// can expose a private capacity pool.
func TestNormalizeGroupInputRejectsUnknownVisibility(t *testing.T) {
	_, err := normalizeGroupInput(
		"fixture-group",
		domain.GroupVisibility("unknown"),
		nil,
		nil,
		domain.GroupRoutingModeAccounts,
		nil,
	)
	require.ErrorIs(t, err, ErrInvalidInput)
}

// Account PUT is allowed to carry an omitted/zero-value status from clients
// that only edit routing fields. A non-empty unknown value must still be
// rejected instead of persisting an account status outside the scheduler
// states.
func TestValidateAccountRejectsUnknownStatus(t *testing.T) {
	account := &domain.Account{
		Name:           "fixture-account",
		TemplateID:     1,
		Status:         domain.AccountStatus("unknown"),
		MaxConcurrency: 8,
	}
	require.ErrorIs(t, validateAccount(account), ErrInvalidInput)
}

func TestSoftDeletedAccountCannotBeUpdatedOrDeleted(t *testing.T) {
	svc, fs, _ := newTask4Svc()
	ctx := context.Background()
	account := &domain.Account{
		Name:           "deleted-account",
		TemplateID:     1,
		Status:         domain.StatusActive,
		UpstreamKey:    "fixture-key",
		MaxConcurrency: 8,
	}
	created, err := fs.CreateAccount(ctx, account)
	require.NoError(t, err)
	deletedAt := time.Now()
	fs.accs[created.ID].DeletedAt = &deletedAt

	_, err = svc.UpdateAccount(ctx, &domain.Account{
		ID:             created.ID,
		Name:           "changed",
		TemplateID:     1,
		Status:         domain.StatusActive,
		UpstreamKey:    "fixture-key",
		MaxConcurrency: 8,
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, svc.DeleteAccount(ctx, created.ID), ErrNotFound)
}

func TestSoftDeletedGroupCannotBeUpdatedOrDeleted(t *testing.T) {
	svc, fs, _ := newTask4Svc()
	ctx := context.Background()
	group, err := fs.CreateGroup(ctx, &domain.Group{
		Name:       "deleted-group",
		Visibility: domain.GroupVisibilityPublic,
	})
	require.NoError(t, err)
	require.NoError(t, svc.DeleteGroup(ctx, group.ID))

	_, err = svc.UpdateGroup(ctx, &domain.Group{
		ID:              group.ID,
		Name:            "changed",
		Visibility:      domain.GroupVisibilityPublic,
		PriceMultiplier: 10000,
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, svc.DeleteGroup(ctx, group.ID), ErrNotFound)
}

func TestUpdateGroupRejectsUnknownVisibility(t *testing.T) {
	svc, fs, _ := newTask4Svc()
	ctx := context.Background()
	group, err := fs.CreateGroup(ctx, &domain.Group{
		Name:       "visibility-group",
		Visibility: domain.GroupVisibilityPublic,
	})
	require.NoError(t, err)

	_, err = svc.UpdateGroup(ctx, &domain.Group{
		ID:              group.ID,
		Name:            group.Name,
		Visibility:      domain.GroupVisibility("unknown"),
		PriceMultiplier: 10000,
	})
	require.ErrorIs(t, err, ErrInvalidInput)
}
