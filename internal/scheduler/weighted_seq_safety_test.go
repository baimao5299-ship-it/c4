// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestNewWeightedSeqSkipsNonPositiveWeights(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	ws := newWeightedSeq([]*accountSnapshot{
		mkAcc(1, -1, tpl),
		mkAcc(2, 0, tpl),
		mkAcc(3, 10, tpl),
	})
	require.NotEmpty(t, ws.seq)
	for _, account := range ws.seq {
		require.Equal(t, int64(3), account.static.Load().acc.ID)
	}
}

func TestNewWeightedSeqSaturatesLargeWeightSum(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	require.Greater(t, maxInt, 2)
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	ws := newWeightedSeq([]*accountSnapshot{
		mkAcc(1, maxInt, tpl),
		mkAcc(2, maxInt-2, tpl),
	})
	require.NotEmpty(t, ws.seq)
	require.LessOrEqual(t, len(ws.seq), maxSeqLen+2)
	seen := map[int64]bool{}
	for _, account := range ws.seq {
		seen[account.static.Load().acc.ID] = true
	}
	require.True(t, seen[1])
	require.True(t, seen[2])
}
