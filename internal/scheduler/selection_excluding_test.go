// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestSelectExcludingSkipsAccountsAlreadyTried(t *testing.T) {
	chat := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	s := newTestScheduler(t, []*domain.Account{acc(1, chat, 4), acc(2, chat, 4)})

	first, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	s.Release(first.AccountID)

	second, err := s.SelectExcluding(10, domain.FormatOpenAIChat, "gpt-4o", []int64{first.AccountID})
	require.NoError(t, err)
	require.NotEqual(t, first.AccountID, second.AccountID)
	s.Release(second.AccountID)

	_, err = s.SelectExcluding(10, domain.FormatOpenAIChat, "gpt-4o", []int64{1, 2})
	require.ErrorIs(t, err, ErrNoAvailable)
}

func TestSelectExcludingPreservesSelectSemanticsWithoutExclusions(t *testing.T) {
	chat := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	s := newTestScheduler(t, []*domain.Account{acc(1, chat, 4)})

	sel, err := s.SelectExcluding(10, domain.FormatOpenAIChat, "gpt-4o", nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)
}
