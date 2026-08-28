// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func upstreamIDPtr(id int64) *int64 { return &id }

func upstreamKeyPtr(key string) *string { return &key }

func TestSelectBoundUpstreamEndpointAndCredential(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"gpt-5"})
	upstream := &domain.Upstream{
		ID:          42,
		BaseURL:     " https://relay.example.test/api/ ",
		UpstreamKey: upstreamKeyPtr("relay-key"),
		Enabled:     true,
	}

	t.Run("account key wins", func(t *testing.T) {
		a := acc(1, tplx, 2)
		a.UpstreamID = upstreamIDPtr(upstream.ID)
		a.Upstream = upstream
		a.UpstreamKey = "account-key"
		s := newTestScheduler(t, []*domain.Account{a})

		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
		require.NoError(t, err)
		require.Equal(t, "https://relay.example.test/api", sel.BaseURL)
		require.Equal(t, "account-key", sel.UpstreamKey)
		s.Release(sel.AccountID)
	})

	t.Run("upstream key fills empty account key", func(t *testing.T) {
		a := acc(2, tplx, 2)
		a.UpstreamID = upstreamIDPtr(upstream.ID)
		a.Upstream = upstream
		a.UpstreamKey = ""
		s := newTestScheduler(t, []*domain.Account{a})

		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
		require.NoError(t, err)
		require.Equal(t, "https://relay.example.test/api", sel.BaseURL)
		require.Equal(t, "relay-key", sel.UpstreamKey)
		s.Release(sel.AccountID)
	})
}

func TestSelectBoundUpstreamUnavailable(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"gpt-5"})
	now := time.Now()
	cases := []struct {
		name     string
		upstream *domain.Upstream
	}{
		{name: "missing row", upstream: nil},
		{name: "disabled", upstream: &domain.Upstream{ID: 2, BaseURL: "https://relay.example.test", Enabled: false}},
		{name: "deleted", upstream: &domain.Upstream{ID: 3, BaseURL: "https://relay.example.test", Enabled: true, DeletedAt: &now}},
		{name: "empty endpoint", upstream: &domain.Upstream{ID: 4, BaseURL: "  ", Enabled: true}},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := acc(int64(i+1), tplx, 2)
			a.UpstreamID = upstreamIDPtr(int64(i + 1))
			a.Upstream = tc.upstream
			s := newTestScheduler(t, []*domain.Account{a})

			_, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
			require.ErrorIs(t, err, ErrNoAvailable)
			byID := s.store.byID.Load().(map[int64]*accountSnapshot)
			st := byID[a.ID].static.Load()
			require.False(t, st.upstreamEnabled)
		})
	}
}

func TestInvalidateGroupPreservesCrossGroupMembership(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"gpt-5"})
	a := acc(1, tplx, 2)
	m := newMemLoader(map[int64][]*domain.Account{
		10: {a},
		20: {a},
	})
	s := newSched(t, m)

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{acc(1, tplx, 1)}
	m.mu.Unlock()
	s.InvalidateGroup(10)

	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	groups := s.store.groups.Load().(map[int64]*groupSnapshot)
	got := byID[1]
	require.Equal(t, []int64{10, 20}, got.static.Load().groupIDs)
	require.Same(t, got, groups[10].accounts[0])
	require.Same(t, got, groups[20].accounts[0])
	require.Equal(t, 1, got.static.Load().acc.MaxConcurrency)

	sel, err := s.Select(20, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	s.Release(sel.AccountID)
}
