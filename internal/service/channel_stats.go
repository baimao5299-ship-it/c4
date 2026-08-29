// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// PublicChannelStatsStore is deliberately optional: existing lightweight
// integrations keep compiling, while the production repository supplies the
// SQL aggregate backed by usage_logs and err_logs.
type PublicChannelStatsStore interface {
	ScanPublicChannelStats(context.Context, []int64, time.Time, time.Time) (map[int64]*domain.PublicChannelStat, error)
}

const maxChannelMonitorSpan = 7 * 24 * time.Hour

// PublicChannelMetric is the user-safe projection for one public group.
// SuccessRate is a percentage in [0,100], and AverageLatencyMS is zero when
// the recent sample has no positive latency values.
type PublicChannelMetric struct {
	Group            *domain.Group
	RequestCount     int64
	ErrorCount       int64
	AverageLatencyMS int64
	SuccessRate      float64
	LastCalledAt     *time.Time
	Status           string
}

// UserChannelMetrics lists only public, live groups and joins their recent
// aggregate telemetry. Private groups are filtered after the authoritative
// group lookup, so a user cannot turn an assigned private group into a public
// monitoring entry by changing query parameters.
func (s *Service) UserChannelMetrics(ctx context.Context, userID int64, from, to time.Time) ([]*PublicChannelMetric, error) {
	if from.IsZero() && to.IsZero() {
		to = time.Now().UTC()
		from = to.Add(-24 * time.Hour)
	}
	if from.IsZero() || to.IsZero() || !to.After(from) || to.Sub(from) > maxChannelMonitorSpan {
		return nil, ErrInvalidInput
	}
	groups, err := s.store.ListGroupsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	public := make([]*domain.Group, 0, len(groups))
	ids := make([]int64, 0, len(groups))
	for _, g := range groups {
		if g == nil || g.DeletedAt != nil || g.Visibility != domain.GroupVisibilityPublic || g.ID <= 0 {
			continue
		}
		public = append(public, g)
		ids = append(ids, g.ID)
	}
	telemetry, ok := s.store.(PublicChannelStatsStore)
	if !ok {
		return nil, fmt.Errorf("public channel stats store is not configured")
	}
	stats, err := telemetry.ScanPublicChannelStats(ctx, ids, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]*PublicChannelMetric, 0, len(public))
	for _, g := range public {
		stat := stats[g.ID]
		metric := &PublicChannelMetric{Group: g, Status: "no_data"}
		if stat != nil {
			metric.RequestCount = stat.RequestCount
			metric.ErrorCount = stat.ErrorCount
			metric.LastCalledAt = stat.LastCalledAt
			if stat.LatencySampleCount > 0 {
				metric.AverageLatencyMS = stat.LatencyTotalMS / stat.LatencySampleCount
			}
			if metric.RequestCount > 0 {
				successes := metric.RequestCount - metric.ErrorCount
				if successes < 0 {
					successes = 0
				}
				metric.SuccessRate = float64(successes) * 100 / float64(metric.RequestCount)
				metric.Status = "stable"
				if metric.SuccessRate < 95 || (metric.AverageLatencyMS > 0 && metric.AverageLatencyMS > 3000) {
					metric.Status = "degraded"
				}
			}
		}
		out = append(out, metric)
	}
	slices.SortStableFunc(out, func(a, b *PublicChannelMetric) int {
		if a.LastCalledAt != nil || b.LastCalledAt != nil {
			if a.LastCalledAt == nil {
				return 1
			}
			if b.LastCalledAt == nil {
				return -1
			}
			if !a.LastCalledAt.Equal(*b.LastCalledAt) {
				if a.LastCalledAt.After(*b.LastCalledAt) {
					return -1
				}
				return 1
			}
		}
		return strings.Compare(a.Group.Name, b.Group.Name)
	})
	return out, nil
}
