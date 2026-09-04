// SPDX-License-Identifier: AGPL-3.0-or-later

package repository_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestPGDisplayOrderLifecycle(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	groups := make([]*domain.Group, 0, 3)
	for _, name := range []string{"display-old", "display-middle", "display-new"} {
		created, err := repos.Groups.CreateGroup(ctx, &domain.Group{
			Name: name, Category: "模型专区", Visibility: domain.GroupVisibilityPublic,
		})
		require.NoError(t, err)
		groups = append(groups, created)
	}
	listed, total, err := repos.Groups.ListGroups(ctx, repository.ListQuery{Limit: 20, Sort: "display_order", Order: "asc"})
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Equal(t, []int64{groups[2].ID, groups[1].ID, groups[0].ID}, groupIDs(listed))
	require.Equal(t, "模型专区", listed[0].Category)

	wanted := []int64{groups[0].ID, groups[2].ID, groups[1].ID}
	require.NoError(t, repos.Groups.ReorderGroups(ctx, wanted))
	listed, _, err = repos.Groups.ListGroups(ctx, repository.ListQuery{Limit: 20, Sort: "display_order", Order: "asc"})
	require.NoError(t, err)
	require.Equal(t, wanted, groupIDs(listed))

	upstream, err := repos.Upstreams.CreateUpstream(ctx, &domain.Upstream{
		Name: "display-member", BaseURL: "https://display.example.com", MultiplierBP: 10000, Enabled: true,
	})
	require.NoError(t, err)
	current, err := repos.Groups.GetGroup(ctx, groups[0].ID)
	require.NoError(t, err)
	require.NotNil(t, current.DisplayOrder)
	orderBefore := *current.DisplayOrder
	staleOrder := orderBefore + 12345
	current.DisplayOrder = &staleOrder
	current.RoutingMode = domain.GroupRoutingModeUpstreams
	current.AllowedModels = []string{"gpt-test"}
	updated, err := repos.Groups.UpdateGroupWithUpstreams(ctx, current, []*domain.GroupUpstream{{
		UpstreamID: upstream.ID, Weight: 100, MaxConcurrency: 80, Enabled: true,
	}})
	require.NoError(t, err)
	require.NotNil(t, updated.DisplayOrder)
	require.Equal(t, orderBefore, *updated.DisplayOrder)
}

func TestPGConcurrentGroupReordersSerialize(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	groups := make([]*domain.Group, 0, 3)
	for _, name := range []string{"concurrent-a", "concurrent-b", "concurrent-c"} {
		created, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: name, Visibility: domain.GroupVisibilityPublic})
		require.NoError(t, err)
		groups = append(groups, created)
	}
	a := []int64{groups[0].ID, groups[1].ID, groups[2].ID}
	b := []int64{groups[2].ID, groups[0].ID, groups[1].ID}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, order := range [][]int64{a, b} {
		order := append([]int64(nil), order...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- repos.Groups.ReorderGroups(ctx, order)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	listed, _, err := repos.Groups.ListGroups(ctx, repository.ListQuery{Limit: 20, Sort: "display_order", Order: "asc"})
	require.NoError(t, err)
	got := groupIDs(listed)
	require.True(t, equalInt64s(got, a) || equalInt64s(got, b), "serialized result must match one complete request: %v", got)
}

func groupIDs(groups []*domain.Group) []int64 {
	ids := make([]int64, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	return ids
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
