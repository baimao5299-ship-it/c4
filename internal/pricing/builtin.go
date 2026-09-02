// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	_ "embed"
	"sort"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// officialSnapshotCSV is a small, provider-only catalogue shipped with the
// server. It is a seed and fallback for model IDs that the live LiteLLM feed
// exposes only under provider-qualified names (for example kimi-k3 or
// claude-opus-4-6). The live feed remains the primary source and can add newer
// rows on the next scheduled sync.
//
//go:embed data/official_usd_per_1m_2026-09-03.csv
var officialSnapshotCSV []byte

func builtinOfficialPrices(log *logx.Logger) *FetchResult {
	result, err := ParseCSV(officialSnapshotCSV, log)
	if err != nil {
		if log != nil {
			log.Warn("built-in pricing snapshot unavailable", logx.Error(err))
		}
		return nil
	}
	return result
}

// mergeWithBuiltinPrices adds missing official rows and fills omitted price
// components conservatively. It is intentionally applied at fetch time so the
// normal snapshot transaction persists the same data used by billing.
func mergeWithBuiltinPrices(primary, fallback *FetchResult) *FetchResult {
	if primary == nil || fallback == nil {
		return primary
	}
	byModel := make(map[string]*domain.PriceEntry, len(primary.PriceEntries)+len(fallback.PriceEntries))
	for _, row := range primary.PriceEntries {
		if row != nil {
			byModel[row.Model] = row
		}
	}
	for _, row := range fallback.PriceEntries {
		if row == nil || row.Model == "" {
			continue
		}
		if existing, ok := byModel[row.Model]; ok {
			byModel[row.Model] = mergeCSVEntriesConservative(existing, row)
		} else {
			byModel[row.Model] = row
		}
	}
	models := append([]string(nil), primary.Models...)
	for _, model := range fallback.Models {
		models = appendUniqueModel(models, model)
	}
	entries := make([]*domain.PriceEntry, 0, len(byModel))
	for _, row := range byModel {
		entries = append(entries, row)
	}
	// Parse/worker reconciliation expects stable model ordering.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Model < entries[j].Model })
	sort.Strings(models)
	return &FetchResult{PriceEntries: entries, Variants: primary.Variants, Models: models, Skipped: primary.Skipped}
}
