// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// /ops/workers scheduler Stats 与真实状态一致性单测（writeCh 占用零成本采集；
// typed struct 断言）。

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

func TestSchedulerStats(t *testing.T) {
	s := New(testCfg(), newMemLoader(nil), rule.New(rule.Config{}, &fakeRuleStore{}, nil), nil)
	st := s.Stats().(SchedulerStats)
	require.Zero(t, st.PendingWritebacks)
	require.Equal(t, 4096, st.WritebackCap)

	s.writeCh <- statusWrite{id: 1, status: domain.Status429}
	s.writeCh <- statusWrite{id: 2, status: domain.StatusActive}
	st = s.Stats().(SchedulerStats)
	require.Equal(t, 2, st.PendingWritebacks, "writeCh 占用 = 实际积压回写")
}
