// SPDX-License-Identifier: AGPL-3.0-or-later

package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReorderedRanksPermutesExistingPageSlots(t *testing.T) {
	stored := int64(-2_500_000)
	current := map[int64]*int64{
		5: nil,
		4: &stored,
		3: nil,
	}

	ranks, err := reorderedRanks([]int64{3, 5, 4}, current)
	require.NoError(t, err)
	require.Equal(t, int64(-5_000_000), ranks[3])
	require.Equal(t, int64(-3_000_000), ranks[5])
	require.Equal(t, int64(-2_500_000), ranks[4])
	require.Equal(t, stored, *current[4], "reordering must not mutate the source snapshot")
}

func TestReorderedRanksRejectsInvalidLists(t *testing.T) {
	tests := []struct {
		name    string
		ids     []int64
		current map[int64]*int64
	}{
		{name: "too short", ids: []int64{1}, current: map[int64]*int64{1: nil}},
		{name: "duplicate", ids: []int64{1, 1}, current: map[int64]*int64{1: nil}},
		{name: "non positive", ids: []int64{0, 1}, current: map[int64]*int64{0: nil, 1: nil}},
		{name: "missing", ids: []int64{1, 2}, current: map[int64]*int64{1: nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := reorderedRanks(tt.ids, tt.current)
			require.Error(t, err)
		})
	}
}
