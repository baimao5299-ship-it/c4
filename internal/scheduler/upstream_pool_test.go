// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package scheduler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// upstreamMemLoader is intentionally local to these tests. Account snapshots
// and upstream-pool snapshots are loaded through the same scheduler reload,
// matching the repository's optional UpstreamPoolLoader contract.
type upstreamMemLoader struct {
	*memLoader
	mu      sync.Mutex
	configs map[int64]*domain.Group
	fail    error
}

func (m *upstreamMemLoader) LoadGroupsUpstreamConfig(context.Context) (map[int64]*domain.Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	out := make(map[int64]*domain.Group, len(m.configs))
	for id, group := range m.configs {
		out[id] = group
	}
	return out, nil
}

func TestInvalidateGroupKeepsUpstreamRoutesOnConfigReadFailure(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 1, 8)
	s, loader := newUpstreamScheduler(t, map[int64]*domain.Group{10: testUpstreamGroup(10, a)})
	loader.fail = fmt.Errorf("temporary config read failure")

	// A targeted account reload must not publish the zero-value account group
	// until its companion upstream configuration is available.
	s.InvalidateGroup(10)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	require.Equal(t, TargetKindUpstreamMember, sel.TargetKind)
	s.ReleaseSelection(sel)
}

func newUpstreamScheduler(t *testing.T, groups map[int64]*domain.Group) (*Scheduler, *upstreamMemLoader) {
	t.Helper()
	byGroup := make(map[int64][]*domain.Account, len(groups))
	for id := range groups {
		byGroup[id] = nil
	}
	loader := &upstreamMemLoader{memLoader: newMemLoader(byGroup), configs: groups}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), loader, re, nil)
	require.NoError(t, s.reload(context.Background()))
	return s, loader
}

func testUpstream(id int64) *domain.Upstream {
	key := fmt.Sprintf("upstream-key-%d", id)
	checked := time.Now()
	return &domain.Upstream{ID: id, Name: fmt.Sprintf("upstream-%d", id), BaseURL: fmt.Sprintf("https://upstream-%d.example", id), UpstreamKey: &key, Models: []string{"gpt-5"}, ModelsCheckedAt: &checked, Enabled: true}
}

func testGroupUpstream(id, upstreamID int64, weight, priority, maxConcurrency int) *domain.GroupUpstream {
	return &domain.GroupUpstream{ID: id, UpstreamID: upstreamID, Upstream: testUpstream(upstreamID), Weight: weight, Priority: priority, MaxConcurrency: maxConcurrency, Enabled: true}
}

func testUpstreamGroup(id int64, members ...*domain.GroupUpstream) *domain.Group {
	return &domain.Group{ID: id, Name: fmt.Sprintf("group-%d", id), RoutingMode: domain.GroupRoutingModeUpstreams, AllowedModels: []string{"gpt-5"}, UpstreamMembers: members}
}

func selectAndReleaseUpstream(t *testing.T, s *Scheduler, groupID int64) *Selection {
	t.Helper()
	sel, err := s.Select(groupID, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	require.Equal(t, TargetKindUpstreamMember, sel.TargetKind)
	s.ReleaseSelection(sel)
	return sel
}

func TestUpstreamPriorityAndWeightedRoundRobin(t *testing.T) {
	a := testGroupUpstream(1, 101, 3, 10, 8)
	b := testGroupUpstream(2, 102, 1, 10, 8)
	// A higher numeric priority is a fallback only. It must not receive traffic
	// while any member at the minimum priority is eligible.
	c := testGroupUpstream(3, 103, 100, 20, 8)
	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{10: testUpstreamGroup(10, a, b, c)})

	counts := map[int64]int{}
	for i := 0; i < 80; i++ {
		sel := selectAndReleaseUpstream(t, s, 10)
		counts[sel.TargetID]++
	}
	require.Equal(t, 60, counts[a.ID], "3:1 weights are preserved in the bounded sequence")
	require.Equal(t, 20, counts[b.ID], "3:1 weights are preserved in the bounded sequence")
	require.Zero(t, counts[c.ID], "higher-priority number is fallback only")

	// A request retry excludes the previously attempted logical target.
	first := selectAndReleaseUpstream(t, s, 10)
	second, err := s.SelectExcluding(10, domain.FormatOpenAIChat, "gpt-5", []int64{first.TargetID})
	require.NoError(t, err)
	require.NotEqual(t, first.TargetID, second.TargetID)
	s.ReleaseSelection(second)
	fallback, err := s.SelectExcluding(10, domain.FormatOpenAIChat, "gpt-5", []int64{a.ID, b.ID})
	require.NoError(t, err, "a higher-priority-number member is a fallback when the primary tier is excluded")
	require.Equal(t, c.ID, fallback.TargetID)
	s.ReleaseSelection(fallback)
}

func TestUpstreamBoundedWeightsDoNotStarveMembers(t *testing.T) {
	a := testGroupUpstream(1, 101, 10000, 1, 8)
	b := testGroupUpstream(2, 102, 1, 1, 8)
	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{10: testUpstreamGroup(10, a, b)})

	groups := s.store.groups.Load().(map[int64]*groupSnapshot)
	route := groups[10].upstreamRoutes[routeKey{format: domain.FormatOpenAIChat, model: "gpt-5"}]
	require.NotNil(t, route)
	require.LessOrEqual(t, len(route.seq.seq), maxSeqLen)
	seen := map[int64]bool{}
	for _, item := range route.seq.seq {
		seen[item.member.ID] = true
	}
	require.True(t, seen[a.ID])
	require.True(t, seen[b.ID], "a large weight must not fill the whole capped sequence")
}

func TestUpstreamConcurrencyAndReleaseSelection(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 1, 1)
	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{10: testUpstreamGroup(10, a)})

	first, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	_, err = s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.ErrorIs(t, err, ErrNoAvailable, "max_concurrency=1 is enforced")
	s.ReleaseSelection(first)
	second, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err, "release returns the upstream slot")
	s.ReleaseSelection(second)

	// The nil guard is part of the release contract used by local rejection
	// paths; it must not panic when a caller has no selection to release.
	s.ReleaseSelection(nil)
	s.MarkSelectionResult(nil, rule.KindOK, nil, 200, "", "")
}

func TestAccountReleaseDoesNotUnderflow(t *testing.T) {
	// Keep this guard next to the upstream release tests: failover and local
	// rejection paths share ReleaseSelection, so a duplicate terminal callback
	// must be harmless for both target kinds.
	s := newTestScheduler(t, []*domain.Account{acc(1, tpl(1, domain.FormatOpenAIChat, []string{"gpt-5"}), 2)})
	first, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	second, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	// Releasing the first selection twice must leave the second request's slot
	// accounted for; a third request is therefore rejected until second ends.
	s.ReleaseSelection(first)
	s.ReleaseSelection(first)
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, int64(1), ri.Concurrency)
	third, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err, "the duplicate release left exactly one slot available")
	_, err = s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.ErrorIs(t, err, ErrNoAvailable, "the duplicate release must not free the second request's slot")
	s.ReleaseSelection(second)
	s.ReleaseSelection(third)
	ri, ok = s.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency)
}

func TestRetiredAccountSelectionReleasesOriginalSlot(t *testing.T) {
	// Removing an account from its last group retires its snapshot from byID
	// while an existing request can still be in flight. ReleaseSelection must
	// settle the snapshot that acquired the slot instead of looking up only the
	// current byID map.
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"gpt-5"}), 1)},
	})
	s := newSched(t, m)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	retired := sel.accountRef
	require.NotNil(t, retired)
	require.Equal(t, int64(1), retired.concurrency.Load())

	m.mu.Lock()
	m.byGroup[10] = nil
	m.mu.Unlock()
	s.InvalidateGroup(10)
	_, present := s.store.byID.Load().(map[int64]*accountSnapshot)[sel.AccountID]
	require.False(t, present)

	s.ReleaseSelection(sel)
	require.Zero(t, retired.concurrency.Load())
}

func TestUpstreamCooldownAndFailureFailover(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 1, 8)
	b := testGroupUpstream(2, 102, 1, 1, 8)
	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{10: testUpstreamGroup(10, a, b)})
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	s.timeNow = func() time.Time { return now }

	failed := selectAndReleaseUpstream(t, s, 10)
	s.MarkSelectionResult(failed, rule.Kind5xx, nil, 502, "temporary upstream failure", failed.Model)
	// The failed member is cooled down, so a subsequent request uses its peer.
	other, err := s.SelectExcluding(10, domain.FormatOpenAIChat, "gpt-5", []int64{failed.TargetID})
	require.NoError(t, err)
	require.NotEqual(t, failed.TargetID, other.TargetID)
	s.ReleaseSelection(other)
	_, err = s.SelectExcluding(10, domain.FormatOpenAIChat, "gpt-5", []int64{a.ID, b.ID})
	require.ErrorIs(t, err, ErrNoAvailable)

	// Advance past the local 2-second breaker and verify the cooled member can
	// re-enter the pool. (The peer remains healthy and may be selected first.)
	now = now.Add(3 * time.Second)
	require.Eventually(t, func() bool {
		sel, selectErr := s.SelectExcluding(10, domain.FormatOpenAIChat, "gpt-5", []int64{other.TargetID})
		if selectErr != nil {
			return false
		}
		s.ReleaseSelection(sel)
		return sel.TargetID == failed.TargetID
	}, time.Second, 5*time.Millisecond)
}

func TestUpstreamPersistedCooldownAndReload(t *testing.T) {
	future := time.Date(2026, 8, 28, 15, 0, 0, 0, time.UTC)
	a := testGroupUpstream(1, 101, 1, 1, 8)
	a.CooldownUntil = &future
	group := testUpstreamGroup(10, a)
	s, loader := newUpstreamScheduler(t, map[int64]*domain.Group{10: group})
	s.timeNow = func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) }
	_, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.ErrorIs(t, err, ErrNoAvailable, "persisted relation cooldown is honored on first load")

	// A changed persisted deadline replaces the old state on full reload.
	later := future.Add(time.Hour)
	loader.mu.Lock()
	loader.configs[10].UpstreamMembers[0].CooldownUntil = &later
	loader.mu.Unlock()
	require.NoError(t, s.InvalidateAllSync())
	up := s.store.upstreams.Load().(map[int64]*upstreamSnapshot)[a.ID]
	require.Equal(t, later, *up.statePtr().cooldownUntil)

	// Replacing membership on reload removes the old route and makes the new
	// member immediately selectable.
	b := testGroupUpstream(2, 102, 1, 1, 8)
	loader.mu.Lock()
	loader.configs[10].UpstreamMembers = []*domain.GroupUpstream{b}
	loader.mu.Unlock()
	require.NoError(t, s.InvalidateAllSync())
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	require.Equal(t, b.ID, sel.TargetID)
	s.ReleaseSelection(sel)
	ups := s.store.upstreams.Load().(map[int64]*upstreamSnapshot)
	_, oldPresent := ups[a.ID]
	require.False(t, oldPresent)
}

func TestUpstreamEndpointEditResetsRuntimeState(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 1, 8)
	future := time.Date(2026, 8, 28, 15, 0, 0, 0, time.UTC)
	a.CooldownUntil = &future
	s, loader := newUpstreamScheduler(t, map[int64]*domain.Group{10: testUpstreamGroup(10, a)})
	s.timeNow = func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) }
	_, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.ErrorIs(t, err, ErrNoAvailable)

	// The relation ID remains the same, but editing the endpoint starts a new
	// connection and must not inherit the persisted cooldown from the old one.
	loader.mu.Lock()
	loader.configs[10].UpstreamMembers[0].Upstream.BaseURL = "https://new-upstream.example"
	loader.mu.Unlock()
	require.NoError(t, s.InvalidateAllSync())
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	require.Equal(t, "https://new-upstream.example", sel.BaseURL)
	s.ReleaseSelection(sel)
}

func TestUpstreamReloadSharesInFlightConcurrency(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 1, 1)
	s, loader := newUpstreamScheduler(t, map[int64]*domain.Group{10: testUpstreamGroup(10, a)})
	first, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	old := first.upstreamRef

	// A periodic reload with the same endpoint/key must preserve the live slot,
	// and the old request's release must free the slot in the new route too.
	require.NoError(t, s.InvalidateAllSync())
	current := s.store.upstreams.Load().(map[int64]*upstreamSnapshot)[a.ID]
	require.NotSame(t, old, current)
	_, err = s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.ErrorIs(t, err, ErrNoAvailable)
	s.ReleaseSelection(first)
	second, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	s.ReleaseSelection(second)

	// Keep the loader referenced so this test documents that the reload came
	// from the same stable configuration rather than a membership replacement.
	require.Len(t, loader.configs[10].UpstreamMembers, 1)
}

func TestUpstreamInvalidateGroupRefreshesPriority(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 1, 8)
	b := testGroupUpstream(2, 102, 1, 2, 8)
	group := testUpstreamGroup(10, a, b)
	s, loader := newUpstreamScheduler(t, map[int64]*domain.Group{10: group})
	first := selectAndReleaseUpstream(t, s, 10)
	require.Equal(t, a.ID, first.TargetID)

	loader.mu.Lock()
	loader.configs[10].UpstreamMembers[0].Enabled = false
	loader.mu.Unlock()
	s.InvalidateGroup(10)
	second, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err, "group invalidation reloads upstream membership")
	require.Equal(t, b.ID, second.TargetID)
	s.ReleaseSelection(second)
}

func TestUpstreamGroupReloadDoesNotMutatePublishedGroup(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 1, 8)
	b := testGroupUpstream(2, 102, 1, 1, 8)
	s, loader := newUpstreamScheduler(t, map[int64]*domain.Group{10: testUpstreamGroup(10, a)})
	before := s.store.groups.Load().(map[int64]*groupSnapshot)[10]

	loader.mu.Lock()
	loader.configs[10].UpstreamMembers = []*domain.GroupUpstream{b}
	loader.mu.Unlock()
	s.InvalidateGroup(10)

	after := s.store.groups.Load().(map[int64]*groupSnapshot)[10]
	require.NotSame(t, before, after, "group-level upstream reload must publish a new immutable object")
	require.Len(t, before.upstreams, 1)
	require.Equal(t, a.ID, before.upstreams[0].member.ID)
	require.Len(t, after.upstreams, 1)
	require.Equal(t, b.ID, after.upstreams[0].member.ID)
}

func TestUpstreamReloadReplacesChangedRelationWithoutMutatingOldSnapshot(t *testing.T) {
	first := testGroupUpstream(1, 101, 1, 1, 8)
	group := testUpstreamGroup(10, first)
	s, loader := newUpstreamScheduler(t, map[int64]*domain.Group{10: group})
	before := s.store.groups.Load().(map[int64]*groupSnapshot)[10]
	oldMember := before.upstreams[0].member
	oldUpstream := before.upstreams[0].upstream

	// Repository reloads return fresh domain objects. Replacing the same
	// relation ID must therefore create a new published snapshot rather than
	// rewriting the object retained by in-flight requests.
	updated := testGroupUpstream(1, 101, 7, 2, 4)
	updated.Upstream.BaseURL = "https://changed.example"
	loader.mu.Lock()
	loader.configs[10].UpstreamMembers = []*domain.GroupUpstream{updated}
	loader.mu.Unlock()
	require.NoError(t, s.InvalidateAllSync())

	after := s.store.groups.Load().(map[int64]*groupSnapshot)[10]
	require.NotSame(t, before.upstreams[0], after.upstreams[0])
	require.Same(t, oldMember, before.upstreams[0].member)
	require.Same(t, oldUpstream, before.upstreams[0].upstream)
	require.Equal(t, "https://upstream-101.example", before.upstreams[0].upstream.BaseURL)
	require.Equal(t, "https://changed.example", after.upstreams[0].upstream.BaseURL)
	require.Equal(t, 7, after.upstreams[0].member.Weight)
	require.Equal(t, 2, after.upstreams[0].member.Priority)
}

func TestUpstreamReloadDoesNotCarryStateAcrossRelationIdentity(t *testing.T) {
	first := testGroupUpstream(1, 101, 1, 1, 8)
	first.GroupID = 10
	group := testUpstreamGroup(10, first)
	s, loader := newUpstreamScheduler(t, map[int64]*domain.Group{10: group})
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	s.timeNow = func() time.Time { return now }

	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	s.ReleaseSelection(sel)
	s.MarkSelectionResult(sel, rule.Kind5xx, nil, 502, "temporary", sel.Model)
	old := sel.upstreamRef
	require.NotNil(t, old.statePtr().cooldownUntil)

	// A reused relation ID pointing at a different upstream is a new logical
	// member even when endpoint and key happen to match. Its breaker must start
	// clean, otherwise the old member's cooldown leaks into the replacement.
	replacement := testGroupUpstream(1, 102, 1, 1, 8)
	replacement.GroupID = 10
	replacement.Upstream.BaseURL = first.Upstream.BaseURL
	replacement.Upstream.UpstreamKey = first.Upstream.UpstreamKey
	loader.mu.Lock()
	loader.configs[10].UpstreamMembers = []*domain.GroupUpstream{replacement}
	loader.mu.Unlock()
	require.NoError(t, s.InvalidateAllSync())
	current := s.store.groups.Load().(map[int64]*groupSnapshot)[10].upstreams[0]
	require.NotSame(t, old, current)
	require.Nil(t, current.statePtr().cooldownUntil)
	_, err = s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
}

func TestRetiredUpstreamSelectionReleasesOriginalSlot(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 1, 1)
	b := testGroupUpstream(2, 102, 1, 1, 8)
	s, loader := newUpstreamScheduler(t, map[int64]*domain.Group{10: testUpstreamGroup(10, a)})

	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.upstreamRef.concurrency.Load())
	retired := sel.upstreamRef

	// Remove the selected relation while its request is still in flight. The
	// new snapshot no longer indexes the old member, but the request must still
	// release the slot it acquired from the retired snapshot.
	loader.mu.Lock()
	loader.configs[10].UpstreamMembers = []*domain.GroupUpstream{b}
	loader.mu.Unlock()
	require.NoError(t, s.InvalidateAllSync())
	_, present := s.store.upstreams.Load().(map[int64]*upstreamSnapshot)[a.ID]
	require.False(t, present)

	s.ReleaseSelection(sel)
	require.Equal(t, int64(0), retired.concurrency.Load())
	// Failure accounting must also settle on the retired object without a map
	// lookup; this is harmless for the retired snapshot and prevents stale slots
	// from retaining state if the caller reports after a reload.
	s.MarkSelectionResult(sel, rule.Kind5xx, nil, 502, "retired", "gpt-5")
	require.NotNil(t, retired.statePtr().cooldownUntil)
}
