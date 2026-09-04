// SPDX-License-Identifier: AGPL-3.0-or-later

package repository

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReferralRewardAmount(t *testing.T) {
	require.Equal(t, int64(0), referralRewardAmount(0))
	require.Equal(t, int64(0), referralRewardAmount(19))
	require.Equal(t, int64(1), referralRewardAmount(20))
	require.Equal(t, int64(50_000), referralRewardAmount(1_000_000))
	require.Equal(t, int64(math.MaxInt64/20), referralRewardAmount(math.MaxInt64))
}

func TestCurrentRedemptionCodeFormat(t *testing.T) {
	require.True(t, isCurrentCode("ABCDEFGHIJKL"))
	require.False(t, isCurrentCode("ABCD-EFGH-IJKL"))
	require.False(t, isCurrentCode("ABCDEFGHIJ1L"))
	require.False(t, isCurrentCode("abcdefghijkl"))
}
