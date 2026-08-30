// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"time"
)

// advisoryLockReleaseTimeout bounds the best-effort unlock round trip. The
// release path uses a fresh background context because it must not inherit a
// worker/request cancellation, while still returning within a finite budget.
const advisoryLockReleaseTimeout = 2 * time.Second

// releaseAdvisoryLock performs the unlock/return sequence for a dedicated
// pool connection. If the unlock fails, the session state is unknown; closing
// the underlying connection before returning it lets pgxpool destroy the
// resource instead of handing a lock-bearing session to another caller.
// Callbacks keep the failure path testable without a live PostgreSQL server.
func releaseAdvisoryLock(
	unlock func(context.Context) error,
	closeConn func(context.Context) error,
	releaseConn func(),
) {
	unlockCtx, cancel := context.WithTimeout(context.Background(), advisoryLockReleaseTimeout)
	defer cancel()
	if err := unlock(unlockCtx); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), advisoryLockReleaseTimeout)
		_ = closeConn(closeCtx)
		closeCancel()
	}
	releaseConn()
}
