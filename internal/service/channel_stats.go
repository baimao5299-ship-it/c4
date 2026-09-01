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
	"github.com/is7qin/c3api/pkg/logx"
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
	PriceMultiplier  int
	ModelPrices      []PublicChannelModelPrice
	RequestCount     int64
	ErrorCount       int64
	AverageLatencyMS int64
	SuccessRate      float64
	LastCalledAt     *time.Time
	Status           string
}

// PublicChannelModelPrice is the pricing catalogue projection for one model
// exposed by a public group. Values keep the pricing snapshot's internal unit
// (milli-USD per million tokens, or milli-USD per call/image) until the HTTP
// handler converts them to user-facing USD and applies the group multiplier.
// A model remains in this list when its catalogue row is missing; nil prices
// then make the missing configuration explicit instead of hiding the model.
type PublicChannelModelPrice struct {
	Model string
	Mode  domain.PriceMode

	// The existing fields are the resolved catalogue values before the public
	// group's multiplier is applied. The HTTP layer turns them into the amount
	// the user is charged. Keep them for compatibility with existing clients.
	InputPerM      *int64
	OutputPerM     *int64
	CacheReadPerM  *int64
	CacheWritePerM *int64
	PricePerCall   *int64
	PricePerImage  *int64
	ImgInTokPerM   *int64
	ImgOutTokPerM  *int64

	// Official* is the raw catalogue row before any conditional variant or
	// group/assignment multiplier. This is deliberately separate from the
	// resolved fields: deriving an original price by dividing an effective
	// price is wrong for manual prices, tier variants, and free groups.
	OfficialInputPerM      *int64
	OfficialOutputPerM     *int64
	OfficialCacheReadPerM  *int64
	OfficialCacheWritePerM *int64
	OfficialPricePerCall   *int64
	OfficialPricePerImage  *int64
	OfficialImgInTokPerM   *int64
	OfficialImgOutTokPerM  *int64
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
	// Reuse the user-facing group projection so per-user assignment
	// multipliers match the amount billing actually charges.
	groups, err := s.ListGroupsForUser(ctx, userID)
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
	// Pricing is read from the immutable service snapshot so refreshing the
	// public monitor does not issue one database query per model. If the
	// snapshot is temporarily unavailable, keep the health monitor usable and
	// expose models with nil prices; the next refresh fills them in.
	allModels := make([]string, 0)
	for _, g := range public {
		allModels = append(allModels, g.AllowedModels...)
	}
	// Read the official catalogue and resolved runtime prices from one immutable
	// snapshot so a concurrent pricing reload cannot mix two versions in one
	// response.
	catalogue, prices, priceErr := s.pricingProjectionForModels(ctx, allModels, "auto", 0, time.Now())
	if priceErr != nil {
		catalogue = make(map[string]*domain.PriceEntry)
		prices = make(map[string]domain.ResolvedPrices)
		if s.log != nil {
			s.log.Warn("public channel pricing snapshot unavailable", logx.Error(priceErr))
		}
	}
	// Resolve aliases once per response. Upstream model IDs often carry a
	// provider namespace or a dated snapshot suffix while the pricing catalogue
	// stores one canonical root row. The lookup helper only accepts a unique,
	// deterministic candidate, so an ambiguous provider price remains visible
	// as unpriced instead of showing a misleading number.
	catalogueKeys := sortedModelKeys(catalogue)
	priceKeys := sortedModelKeys(prices)
	assignments, err := s.store.ListAssignmentsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	effectiveMultipliers := make(map[int64]int, len(assignments))
	for _, assignment := range assignments {
		if assignment != nil && assignment.PriceMultiplier != nil {
			effectiveMultipliers[assignment.GroupID] = *assignment.PriceMultiplier
		}
	}
	out := make([]*PublicChannelMetric, 0, len(public))
	for _, g := range public {
		stat := stats[g.ID]
		multiplier := g.PriceMultiplier
		if userMultiplier, ok := effectiveMultipliers[g.ID]; ok {
			multiplier = userMultiplier
		}
		metric := &PublicChannelMetric{Group: g, PriceMultiplier: multiplier, Status: "no_data", ModelPrices: make([]PublicChannelModelPrice, 0, len(g.AllowedModels))}
		for _, model := range g.AllowedModels {
			row := PublicChannelModelPrice{Model: model}
			catalogueKey, hasCatalogue := modelLookupKey(catalogueKeys, model)
			if entry, ok := catalogue[catalogueKey]; hasCatalogue && ok && entry != nil {
				row.OfficialInputPerM = entry.InputPerM
				row.OfficialOutputPerM = entry.OutputPerM
				row.OfficialCacheReadPerM = entry.CacheReadPerM
				row.OfficialCacheWritePerM = entry.CacheWritePerM
				row.OfficialPricePerCall = entry.PricePerCall
				row.OfficialPricePerImage = entry.PricePerImage
				row.OfficialImgInTokPerM = entry.ImgInTokPerM
				row.OfficialImgOutTokPerM = entry.ImgOutTokPerM
				row.Mode = entry.Mode
			}
			priceKey, hasPrice := modelLookupKey(priceKeys, model)
			if entry, ok := prices[priceKey]; hasPrice && ok {
				row.Mode = entry.Mode
				row.InputPerM = entry.InputPerM
				row.OutputPerM = entry.OutputPerM
				row.CacheReadPerM = entry.CacheReadPerM
				row.CacheWritePerM = entry.CacheWritePerM
				row.PricePerCall = entry.PricePerCall
				row.PricePerImage = entry.PricePerImage
				row.ImgInTokPerM = entry.ImgInTokPerM
				row.ImgOutTokPerM = entry.ImgOutTokPerM
			}
			metric.ModelPrices = append(metric.ModelPrices, row)
		}
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
