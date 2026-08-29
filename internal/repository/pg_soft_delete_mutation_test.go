// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// Soft-deleted management rows are audit-only. They must not be accepted by
// either single-row or batch mutation paths, otherwise a second delete looks
// successful and a batch edit can silently mutate an object absent from lists.
func TestPGSoftDeletedRowsRejectMutation(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("template", func(t *testing.T) {
		tpl := seedPGTemplateNamed(t, repos, "sd-mutation-template")
		require.NoError(t, repos.DeleteTemplate(ctx, tpl.ID))

		got, err := repos.GetTemplate(ctx, tpl.ID)
		require.NoError(t, err)
		got.Name = "should-not-update"
		_, err = repos.Templates.UpdateTemplate(ctx, got)
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.ErrorIs(t, repos.Templates.UpdateTemplatesBatch(ctx, []int64{tpl.ID}, repository.TemplatePatch{}), repository.ErrNotFound)
		require.ErrorIs(t, repos.DeleteTemplatesBatch(ctx, []int64{tpl.ID}), repository.ErrNotFound)
	})

	t.Run("group", func(t *testing.T) {
		g := seedPGGroup(t, repos, "sd-mutation-group")
		require.NoError(t, repos.DeleteGroup(ctx, g.ID))

		got, err := repos.GetGroup(ctx, g.ID)
		require.NoError(t, err)
		got.Name = "should-not-update"
		_, err = repos.Groups.UpdateGroup(ctx, got)
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.ErrorIs(t, repos.Groups.UpdateGroupsBatch(ctx, []int64{g.ID}, repository.GroupPatch{}), repository.ErrNotFound)
		require.ErrorIs(t, repos.DeleteGroupsBatch(ctx, []int64{g.ID}), repository.ErrNotFound)
	})

	t.Run("account", func(t *testing.T) {
		tpl := seedPGTemplateNamed(t, repos, "sd-mutation-account-template")
		acc := seedPGAccount(t, repos, tpl.ID, "sd-mutation-account")
		require.NoError(t, repos.DeleteAccount(ctx, acc.ID))

		got, err := repos.GetAccount(ctx, acc.ID)
		require.NoError(t, err)
		got.Name = "should-not-update"
		_, err = repos.Accounts.UpdateAccount(ctx, got, nil)
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.NoError(t, repos.Accounts.UpdateAccountStatus(ctx, acc.ID, domain.Status429, nil, nil, nil), "stale scheduler writeback is a no-op")
		after, err := repos.GetAccount(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, domain.StatusActive, after.Status, "soft-deleted account status remains audit state")
		require.ErrorIs(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, nil), repository.ErrNotFound)
		require.ErrorIs(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{}), repository.ErrNotFound)
		require.ErrorIs(t, repos.DeleteAccountsBatch(ctx, []int64{acc.ID}), repository.ErrNotFound)
	})

	t.Run("deleted group relations", func(t *testing.T) {
		tpl := seedPGTemplateNamed(t, repos, "sd-group-relation-template")
		acc := seedPGAccount(t, repos, tpl.ID, "sd-group-relation-account")
		user := seedPGUser(t, repos, "sd-group-relation@example.com")
		group := seedPGGroup(t, repos, "sd-group-relation")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{group.ID}))
		require.NoError(t, repos.Assignments.Grant(ctx, group.ID, user.ID))
		require.NoError(t, repos.Groups.DeleteGroup(ctx, group.ID))

		groupIDs, err := repos.Accounts.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.Empty(t, groupIDs, "deleted groups must not be returned to account edit forms")
		multiplier := 12000
		require.ErrorIs(t, repos.Assignments.SetMultiplier(ctx, group.ID, user.ID, &multiplier), repository.ErrNotFound)
	})

	t.Run("key", func(t *testing.T) {
		user := seedPGUser(t, repos, "sd-mutation-key@example.com")
		group := seedPGGroup(t, repos, "sd-mutation-key-group")
		created, err := repos.Keys.CreateKey(ctx, &domain.Key{
			UserID: user.ID, GroupID: group.ID, Name: "sd-mutation-key",
			KeyRaw: "sd-mutation-key-raw", Status: domain.KeyStatusActive,
		})
		require.NoError(t, err)
		require.NoError(t, repos.DeleteKey(ctx, created.ID))

		name := "should-not-update"
		_, err = repos.Keys.UpdateKey(ctx, &repository.KeyPatch{ID: created.ID, Name: &name})
		require.ErrorIs(t, err, repository.ErrNotFound)
		_, err = repos.Keys.RotateKey(ctx, created.ID, "should-not-rotate")
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.ErrorIs(t, repos.DeleteKey(ctx, created.ID), repository.ErrNotFound)
	})

	t.Run("rule", func(t *testing.T) {
		id, err := repos.CreateRule(ctx, domain.Rule{Name: "sd-mutation-rule", Enabled: true, Priority: 71001})
		require.NoError(t, err)
		require.NoError(t, repos.DeleteRule(ctx, id))

		err = repos.UpdateRule(ctx, domain.Rule{ID: id, Name: "should-not-update", Enabled: true, Priority: 71002})
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.ErrorIs(t, repos.DeleteRule(ctx, id), repository.ErrNotFound)
		require.ErrorIs(t, repos.DeleteRulesBatch(ctx, []int64{id}), repository.ErrNotFound)
	})
}
