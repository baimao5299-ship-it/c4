// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

func seedScopedTempBalance(t *testing.T, repos *repository.Repository, userID, groupID, amount int64) int64 {
	t.Helper()
	row, err := repos.Client.TempBalance.Create().
		SetUserID(userID).
		SetAmount(amount).
		SetGroupID(groupID).
		SetNillableExpiresAt(ptrTime(time.Now().Add(time.Hour))).
		Save(context.Background())
	require.NoError(t, err)
	return row.ID
}

// TestSettleScopedTempBalanceMatchesUsageGroup proves that a scoped allowance
// is invisible to logs from another group, while the matching log still uses
// it through the FEFO lane.
func TestSettleScopedTempBalanceMatchesUsageGroup(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "scoped-isolation@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	const scopedGroupID int64 = 101
	const otherGroupID int64 = 202
	tempID := seedScopedTempBalance(t, repos, u.ID, scopedGroupID, 50000)

	other := costLogFor(u.ID, "scoped-other", 40000)
	other.GroupID = otherGroupID
	seedUnbilled(t, repos, other)

	res, err := repos.SettleFefoBatch(ctx, 10, 1, 0)
	require.NoError(t, err)
	require.Zero(t, res.BatchRows, "scoped balance must not activate for another group")
	require.Equal(t, int64(50000), tempBalanceAmount(t, repos, tempID))

	matching := costLogFor(u.ID, "scoped-match", 40000)
	matching.GroupID = scopedGroupID
	seedUnbilled(t, repos, matching)

	res, err = repos.SettleFefoBatch(ctx, 10, 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.BatchRows)
	require.Equal(t, int64(1), res.Marked)
	require.Zero(t, res.DebitedUsers, "matching scoped allowance fully covers the log")
	require.Equal(t, int64(10000), tempBalanceAmount(t, repos, tempID))

	// The non-matching log was intentionally left for the balance lane. It must
	// never consume the scoped row, even after the matching log has settled.
	res, err = repos.SettleBalanceBatch(ctx, 10, 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.BatchRows)
	require.Equal(t, int64(1), res.Marked)
	require.Equal(t, int64(10000), tempBalanceAmount(t, repos, tempID))
	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(60000), got.Balance)
}

// TestSettleLegacyGlobalTempBalanceAppliesAcrossGroups preserves the legacy
// NULL group_id behavior: a global temporary allowance remains valid for a
// usage row carrying any group id.
func TestSettleLegacyGlobalTempBalanceAppliesAcrossGroups(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "global-across-groups@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	tempID := seedTempBalance(t, repos, u.ID, 50000, ptrTime(time.Now().Add(time.Hour)))
	log := costLogFor(u.ID, "global-grouped-log", 40000)
	log.GroupID = 909
	seedUnbilled(t, repos, log)

	res, err := repos.SettleFefoBatch(ctx, 10, 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.BatchRows)
	require.Equal(t, int64(1), res.Marked)
	require.Zero(t, res.DebitedUsers)
	require.Equal(t, int64(10000), tempBalanceAmount(t, repos, tempID))
	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100000), got.Balance)
}

// TestSettleGlobalTempBalanceTakesPrecedence documents the mixed-grant policy:
// a positive global row is consumed before a matching scoped row. The scoped
// row is retained for later requests once the global pool is gone.
func TestSettleGlobalTempBalanceTakesPrecedence(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "global-before-scoped@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	const groupID int64 = 717
	globalID := seedTempBalance(t, repos, u.ID, 50000, ptrTime(time.Now().Add(2*time.Hour)))
	scopedID := seedScopedTempBalance(t, repos, u.ID, groupID, 50000)
	log := costLogFor(u.ID, "global-before-scoped-log", 40000)
	log.GroupID = groupID
	seedUnbilled(t, repos, log)

	res, err := repos.SettleFefoBatch(ctx, 10, 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.BatchRows)
	require.Equal(t, int64(10000), tempBalanceAmount(t, repos, globalID))
	require.Equal(t, int64(50000), tempBalanceAmount(t, repos, scopedID), "scoped row waits until global pool is exhausted")
}

// TestSettleScopedTempBalanceAggregatesUserSpill verifies that multiple
// scoped groups for one user produce one summed balance debit. Without the
// user_spill aggregation, UPDATE ... FROM spill can select only one matching
// spill row and silently undercharge the other group.
func TestSettleScopedTempBalanceAggregatesUserSpill(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "scoped-spill@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	const groupA int64 = 303
	const groupB int64 = 404
	a := seedScopedTempBalance(t, repos, u.ID, groupA, 20000)
	b := seedScopedTempBalance(t, repos, u.ID, groupB, 20000)

	logA := costLogFor(u.ID, "spill-a", 30000)
	logA.GroupID = groupA
	seedUnbilled(t, repos, logA)
	logB := costLogFor(u.ID, "spill-b", 30000)
	logB.GroupID = groupB
	seedUnbilled(t, repos, logB)

	res, err := repos.SettleFefoBatch(ctx, 10, 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(2), res.BatchRows)
	require.Equal(t, int64(2), res.Marked)
	require.Equal(t, int64(1), res.DebitedUsers)
	require.Zero(t, res.ForcedUsers)
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, a))
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, b))

	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(80000), got.Balance,
		"two group spills of 10000 each must be aggregated before the user update")
}
