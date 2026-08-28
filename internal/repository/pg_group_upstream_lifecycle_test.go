// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/groupupstream"
	"github.com/is7qin/c3api/internal/repository"
)

func TestPGDeleteGroupRemovesUpstreamMembers(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	up, err := repos.Upstreams.CreateUpstream(ctx, &domain.Upstream{
		Name: "delete-group-upstream", BaseURL: "https://relay.example.com",
		MultiplierBP: 10000, Enabled: true,
	})
	require.NoError(t, err)
	group, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "delete-group-with-upstream", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-test"},
		PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	require.NoError(t, repos.Groups.SetGroupUpstreams(ctx, group.ID, []*domain.GroupUpstream{{
		UpstreamID: up.ID, Weight: 100, Priority: 0, MaxConcurrency: 8, Enabled: true,
	}}))
	count, err := repos.Client.GroupUpstream.Query().Where(groupupstream.GroupIDEQ(group.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	require.NoError(t, repos.Groups.DeleteGroup(ctx, group.ID))
	count, err = repos.Client.GroupUpstream.Query().Where(groupupstream.GroupIDEQ(group.ID)).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, count, "soft-deleting a group must remove its runtime upstream relations")
	got, err := repos.Groups.GetGroup(ctx, group.ID)
	require.NoError(t, err)
	require.NotNil(t, got.DeletedAt)
}

func TestPGDeleteUpstreamRemovesGroupMembers(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	up, err := repos.Upstreams.CreateUpstream(ctx, &domain.Upstream{
		Name: "delete-upstream-with-members", BaseURL: "https://relay.example.com",
		MultiplierBP: 10000, Enabled: true,
	})
	require.NoError(t, err)
	group, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "group-for-delete-upstream", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-test"},
		PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	require.NoError(t, repos.Groups.SetGroupUpstreams(ctx, group.ID, []*domain.GroupUpstream{{
		UpstreamID: up.ID, Weight: 100, Priority: 0, MaxConcurrency: 8, Enabled: true,
	}}))

	require.NoError(t, repos.Upstreams.DeleteUpstream(ctx, up.ID))
	count, err := repos.Client.GroupUpstream.Query().Where(groupupstream.UpstreamIDEQ(up.ID)).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, count, "soft-deleting an upstream must remove all runtime group relations")
	members, err := repos.Groups.ListGroupUpstreams(ctx, group.ID)
	require.NoError(t, err)
	require.Empty(t, members)
}

func TestPGUpdateGroupWithUpstreamsIsAtomic(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	first, err := repos.Upstreams.CreateUpstream(ctx, &domain.Upstream{
		Name: "atomic-update-first", BaseURL: "https://first.example.com",
		MultiplierBP: 10000, Enabled: true,
	})
	require.NoError(t, err)
	second, err := repos.Upstreams.CreateUpstream(ctx, &domain.Upstream{
		Name: "atomic-update-second", BaseURL: "https://second.example.com",
		MultiplierBP: 10000, Enabled: true,
	})
	require.NoError(t, err)
	created, err := repos.Groups.CreateGroupWithUpstreams(ctx, &domain.Group{
		Name: "atomic-update-group", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-old"},
		PriceMultiplier: 10000,
	}, []*domain.GroupUpstream{{UpstreamID: first.ID, Weight: 100, MaxConcurrency: 8, Enabled: true}})
	require.NoError(t, err)

	updated, err := repos.Groups.UpdateGroupWithUpstreams(ctx, &domain.Group{
		ID: created.ID, Name: "atomic-update-group-new", Visibility: domain.GroupVisibilityPrivate,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-new"},
		PriceMultiplier: 12500,
	}, []*domain.GroupUpstream{{UpstreamID: second.ID, Weight: 80, Priority: 1, MaxConcurrency: 12, Enabled: true}})
	require.NoError(t, err)
	require.Equal(t, "atomic-update-group-new", updated.Name)
	require.Equal(t, 12500, updated.PriceMultiplier)
	members, err := repos.Groups.ListGroupUpstreams(ctx, created.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, second.ID, members[0].UpstreamID)

	_, err = repos.Groups.UpdateGroupWithUpstreams(ctx, &domain.Group{
		ID: created.ID, Name: "must-rollback", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-bad"},
		PriceMultiplier: 20000,
	}, []*domain.GroupUpstream{{UpstreamID: 999999, Weight: 100, MaxConcurrency: 8, Enabled: true}})
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrNotFound)
	stored, err := repos.Groups.GetGroup(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "atomic-update-group-new", stored.Name)
	require.Equal(t, 12500, stored.PriceMultiplier)
	members, err = repos.Groups.ListGroupUpstreams(ctx, created.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, second.ID, members[0].UpstreamID)
}
