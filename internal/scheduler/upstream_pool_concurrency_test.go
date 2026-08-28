// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// TestUpstreamSuccessCannotClearConcurrentFailure fixes the ordering where a
// success observes an empty state, a concurrent 5xx publishes a cooldown, and
// the stale success then stores the empty state over it. The gates make that
// interleaving deterministic rather than relying on a scheduler race.
func TestUpstreamSuccessCannotClearConcurrentFailure(t *testing.T) {
	a := testGroupUpstream(1, 101, 1, 1, 8)
	s, _ := newUpstreamScheduler(t, map[int64]*domain.Group{10: testUpstreamGroup(10, a)})
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-5")
	require.NoError(t, err)
	s.ReleaseSelection(sel)
	item := sel.upstreamRef
	require.NotNil(t, item)
	item.state.Store(&upstreamState{})

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	successEntered := make(chan struct{})
	failureEntered := make(chan struct{})
	successGate := make(chan struct{})
	failureGate := make(chan struct{})
	s.timeNow = func() time.Time {
		switch calls.Add(1) {
		case 1:
			close(successEntered)
			<-successGate
		case 2:
			close(failureEntered)
			<-failureGate
		}
		return now
	}

	successDone := make(chan struct{})
	go func() {
		s.MarkSelectionResult(sel, rule.KindOK, nil, 200, "", sel.Model)
		close(successDone)
	}()
	<-successEntered

	failureDone := make(chan struct{})
	go func() {
		s.MarkSelectionResult(sel, rule.Kind5xx, nil, 502, "temporary", sel.Model)
		close(failureDone)
	}()
	<-failureEntered

	// Publish the failure first, then let the stale success attempt to commit.
	close(failureGate)
	<-failureDone
	require.NotNil(t, item.statePtr().cooldownUntil, "failure must publish a cooldown")
	close(successGate)
	<-successDone
	require.NotNil(t, item.statePtr().cooldownUntil, "stale success must not clear a newer cooldown")
}

func TestInvalidateGroupBeforeInitialReload(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"gpt-5"}), 4)},
	})
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)

	// The first NOTIFY may race the initial full reload. It must install the
	// targeted group snapshot without a nil-map panic.
	require.NotPanics(t, func() { s.InvalidateGroup(10) })
	groups, ok := s.store.groups.Load().(map[int64]*groupSnapshot)
	require.True(t, ok)
	require.Contains(t, groups, int64(10))
}
