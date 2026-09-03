// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	_ "embed"
	"sort"
	"strings"
	"time"

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
	return addUniquePreviewDateAliases(result)
}

// addUniquePreviewDateAliases makes a dated preview price usable by relays
// that expose the same model without the preview/date suffix. An alias is
// synthesized only when exactly one priced row maps to it. Existing exact
// rows win, and collisions are left unresolved so billing never guesses.
func addUniquePreviewDateAliases(result *FetchResult) *FetchResult {
	if result == nil {
		return nil
	}
	exact := make(map[string]struct{}, len(result.PriceEntries))
	for _, row := range result.PriceEntries {
		if row != nil && row.Model != "" {
			exact[strings.ToLower(row.Model)] = struct{}{}
		}
	}
	type candidate struct {
		entry    *domain.PriceEntry
		variants []*domain.PriceVariant
		count    int
	}
	variantsByModel := make(map[string][]*domain.PriceVariant)
	for _, variant := range result.Variants {
		if variant != nil {
			variantsByModel[variant.Model] = append(variantsByModel[variant.Model], variant)
		}
	}
	candidates := make(map[string]*candidate)
	for _, row := range result.PriceEntries {
		if row == nil {
			continue
		}
		alias, ok := previewDateAlias(row.Model)
		if !ok {
			continue
		}
		if _, exists := exact[strings.ToLower(alias)]; exists {
			continue
		}
		candidateKey := strings.ToLower(alias)
		item := candidates[candidateKey]
		if item == nil {
			copyEntry := *row
			copyEntry.Model = alias
			item = &candidate{entry: &copyEntry, variants: variantsByModel[row.Model]}
			candidates[candidateKey] = item
		}
		item.count++
	}
	for _, item := range candidates {
		if item.count != 1 {
			continue
		}
		result.PriceEntries = append(result.PriceEntries, item.entry)
		result.Models = appendUniqueModel(result.Models, item.entry.Model)
		for _, source := range item.variants {
			copyVariant := *source
			copyVariant.Model = item.entry.Model
			result.Variants = append(result.Variants, &copyVariant)
		}
	}
	sort.Slice(result.PriceEntries, func(i, j int) bool { return result.PriceEntries[i].Model < result.PriceEntries[j].Model })
	sort.Strings(result.Models)
	return result
}

func previewDateAlias(model string) (string, bool) {
	model = strings.TrimSpace(model)
	lastDash := strings.LastIndexByte(model, '-')
	if lastDash <= 0 || !isCompactDate(model[lastDash+1:]) {
		return "", false
	}
	base := model[:lastDash]
	const marker = "-preview"
	if len(base) <= len(marker) || !strings.EqualFold(base[len(base)-len(marker):], marker) {
		return "", false
	}
	return base[:len(base)-len(marker)], true
}

func isCompactDate(value string) bool {
	if len(value) != 6 && len(value) != 8 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	yearOffset := 4
	if len(value) == 6 {
		// YYMMDD is used by current Volcengine snapshots. Restrict it to a
		// contemporary range so an arbitrary six-digit model suffix is not
		// treated as a release date.
		if value[:2] < "20" || value[:2] > "39" {
			return false
		}
		value = "20" + value
	}
	year := decimal(value[:yearOffset])
	month := decimal(value[yearOffset : yearOffset+2])
	day := decimal(value[yearOffset+2:])
	if year < 1900 || month < 1 || month > 12 || day < 1 || day > 31 {
		return false
	}
	parsed := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	return parsed.Year() == year && int(parsed.Month()) == month && parsed.Day() == day
}

func decimal(value string) int {
	n := 0
	for _, r := range value {
		n = n*10 + int(r-'0')
	}
	return n
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
