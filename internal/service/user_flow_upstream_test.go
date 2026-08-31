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

func TestUserKeyFlowRejectsUnroutableUpstreamGroup(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	ctx := context.Background()
	user, err := store.CreateUser(ctx, &domain.User{Email: "flow@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	group := &domain.Group{
		ID: 1, Name: "relay", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-5"},
	}
	store.groups[group.ID] = group
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	groups, err := svc.ListGroupsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Empty(t, groups, "empty upstream pool must not be offered to a user")
	_, err = svc.CreateKey(ctx, user.ID, "key", group.ID, 0, 0)
	require.ErrorIs(t, err, ErrGroupUnavailable)
	require.ErrorIs(t, err, ErrConflict)

	checkedAt := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	store.upstreams[11] = &domain.Upstream{
		ID: 11, Name: "relay-a", BaseURL: "https://relay.example", Enabled: true,
		Models: []string{"gpt-4"}, ModelsCheckedAt: &checkedAt,
	}
	store.members[group.ID] = []*domain.GroupUpstream{{ID: 21, GroupID: group.ID, UpstreamID: 11, Enabled: true}}
	groups, err = svc.ListGroupsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Empty(t, groups, "a checked catalogue without an allowed model has no route")
	_, err = svc.CreateKey(ctx, user.ID, "key", group.ID, 0, 0)
	require.ErrorIs(t, err, ErrGroupUnavailable)

	store.upstreams[11].Models = []string{"gpt-5"}
	groups, err = svc.ListGroupsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	created, err := svc.CreateKey(ctx, user.ID, "key", group.ID, 0, 0)
	require.NoError(t, err)
	require.Equal(t, group.ID, created.GroupID)
}

func TestUserKeyFlowKeepsUncheckedAndAccountGroupsCompatible(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	ctx := context.Background()
	user, err := store.CreateUser(ctx, &domain.User{Email: "compat@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	accountGroup := &domain.Group{ID: 1, Name: "accounts", Visibility: domain.GroupVisibilityPublic}
	upstreamGroup := &domain.Group{
		ID: 2, Name: "relay", Visibility: domain.GroupVisibilityPublic,
		RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-5"},
	}
	store.groups[accountGroup.ID] = accountGroup
	store.groups[upstreamGroup.ID] = upstreamGroup
	store.upstreams[11] = &domain.Upstream{ID: 11, Name: "unchecked", BaseURL: "https://relay.example", Enabled: true}
	store.members[upstreamGroup.ID] = []*domain.GroupUpstream{{ID: 21, GroupID: upstreamGroup.ID, UpstreamID: 11, Enabled: true}}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)

	groups, err := svc.ListGroupsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Len(t, groups, 2, "legacy unchecked upstream and account groups remain selectable")
	_, err = svc.CreateKey(ctx, user.ID, "relay-key", upstreamGroup.ID, 0, 0)
	require.NoError(t, err)
	_, err = svc.CreateKey(ctx, user.ID, "account-key", accountGroup.ID, 0, 0)
	require.NoError(t, err)
}

func TestListGroupsForUserReturnsEffectiveAssignmentMultiplier(t *testing.T) {
	store := newGroupUpstreamStoreStub()
	ctx := context.Background()
	user, err := store.CreateUser(ctx, &domain.User{Email: "multiplier@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	group := &domain.Group{ID: 1, Name: "public", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 800}
	store.groups[group.ID] = group
	require.NoError(t, store.GrantGroup(ctx, group.ID, user.ID))
	require.NoError(t, store.SetAssignmentMultiplier(ctx, group.ID, user.ID, intPtr(10)))

	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)
	groups, err := svc.ListGroupsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, 10, groups[0].PriceMultiplier, "user-facing price must match the effective x0.001 assignment")
	require.Equal(t, 800, store.groups[group.ID].PriceMultiplier, "projection must not mutate the shared group row")
}

func TestUpstreamGroupHasRouteMatchesPersistentSchedulerRules(t *testing.T) {
	checkedAt := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	member := func(id int64, enabled bool, upstream *domain.Upstream) *domain.GroupUpstream {
		return &domain.GroupUpstream{ID: id, Enabled: enabled, Upstream: upstream}
	}
	upstream := func(enabled bool, baseURL string, checked bool, models ...string) *domain.Upstream {
		u := &domain.Upstream{Enabled: enabled, BaseURL: baseURL, Models: models}
		if checked {
			u.ModelsCheckedAt = &checkedAt
		}
		return u
	}

	cases := []struct {
		name    string
		allowed []string
		members []*domain.GroupUpstream
		want    bool
	}{
		{name: "empty pool", allowed: []string{"gpt-5"}, want: false},
		{name: "disabled relation", allowed: []string{"gpt-5"}, members: []*domain.GroupUpstream{member(1, false, upstream(true, "https://relay.example", false))}, want: false},
		{name: "disabled upstream", allowed: []string{"gpt-5"}, members: []*domain.GroupUpstream{member(1, true, upstream(false, "https://relay.example", false))}, want: false},
		{name: "blank endpoint", allowed: []string{"gpt-5"}, members: []*domain.GroupUpstream{member(1, true, upstream(true, " ", false))}, want: false},
		{name: "unchecked catalogue", allowed: []string{"gpt-5"}, members: []*domain.GroupUpstream{member(1, true, upstream(true, "https://relay.example", false))}, want: true},
		{name: "checked model miss", allowed: []string{"gpt-5"}, members: []*domain.GroupUpstream{member(1, true, upstream(true, "https://relay.example", true, "gpt-4"))}, want: false},
		{name: "checked model hit", allowed: []string{"gpt-5"}, members: []*domain.GroupUpstream{member(1, true, upstream(true, "https://relay.example", true, "gpt-5"))}, want: true},
		{name: "legacy all unchecked", members: []*domain.GroupUpstream{member(1, true, upstream(true, "https://relay.example", false))}, want: true},
		{name: "legacy checked empty", members: []*domain.GroupUpstream{member(1, true, upstream(true, "https://relay.example", true))}, want: false},
		{name: "legacy confirmed intersection", members: []*domain.GroupUpstream{
			member(1, true, upstream(true, "https://a.example", true, "gpt-5", "gpt-4")),
			member(2, true, upstream(true, "https://b.example", true, "gpt-5")),
		}, want: true},
		{name: "legacy disjoint catalogues", members: []*domain.GroupUpstream{
			member(1, true, upstream(true, "https://a.example", true, "gpt-5")),
			member(2, true, upstream(true, "https://b.example", true, "gpt-4")),
		}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, upstreamGroupHasRoute(tc.allowed, tc.members))
		})
	}
}
