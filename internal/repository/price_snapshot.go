// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
)

// ApplyLiteLLMSnapshot publishes base prices, conditional variants, and stale
// row reconciliation in one database transaction. The legacy individual
// methods remain available for embedders, while the production pricing worker
// uses this capability to prevent a failed batch from exposing a mixed old/new
// catalogue after restart.
func (r *Repository) ApplyLiteLLMSnapshot(ctx context.Context, entries []*domain.PriceEntry, variants []*domain.PriceVariant, models []string) (int, int, error) {
	models = normalizeModelList(models)
	if len(models) == 0 {
		return 0, 0, fmt.Errorf("%w: candidate contains no billable models", ErrSuspiciousPriceSnapshot)
	}
	candidateSet := make(map[string]struct{}, len(models))
	for _, model := range models {
		candidateSet[model] = struct{}{}
	}
	orderedEntries := make([]*domain.PriceEntry, 0, len(entries))
	entrySet := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry == nil {
			return 0, 0, fmt.Errorf("pricing: snapshot contains a null price entry")
		}
		model := strings.TrimSpace(entry.Model)
		if model == "" {
			return 0, 0, fmt.Errorf("pricing: snapshot contains an empty price model")
		}
		if _, duplicate := entrySet[model]; duplicate {
			return 0, 0, fmt.Errorf("pricing: snapshot contains duplicate price model %q", model)
		}
		if _, expected := candidateSet[model]; !expected {
			return 0, 0, fmt.Errorf("pricing: price model %q is absent from the candidate snapshot", model)
		}
		copyEntry := *entry
		copyEntry.Model = model
		orderedEntries = append(orderedEntries, &copyEntry)
		entrySet[model] = struct{}{}
	}
	if len(entrySet) != len(candidateSet) {
		return 0, 0, fmt.Errorf("pricing: candidate model count %d does not match price rows %d", len(candidateSet), len(entrySet))
	}
	sort.SliceStable(orderedEntries, func(i, j int) bool { return orderedEntries[i].Model < orderedEntries[j].Model })
	normalizedVariants := make([]*domain.PriceVariant, 0, len(variants))
	for _, variant := range variants {
		if variant == nil {
			return 0, 0, fmt.Errorf("pricing: snapshot contains a null price variant")
		}
		model := strings.TrimSpace(variant.Model)
		if model == "" {
			return 0, 0, fmt.Errorf("pricing: snapshot contains an empty variant model")
		}
		if _, expected := candidateSet[model]; !expected {
			return 0, 0, fmt.Errorf("pricing: variant model %q is absent from the candidate snapshot", model)
		}
		copyVariant := *variant
		copyVariant.Model = model
		normalizedVariants = append(normalizedVariants, &copyVariant)
	}

	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() // nolint:errcheck // Commit closes the transaction.
	drv := &txDriver{tx: tx, drv: r.driver}
	tr := newRepositoryWithLocks(ent.NewClient(ent.Driver(drv)), drv, nil, true)

	manualModels, err := tr.PriceEntries.ManualEntryModels(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("pricing: list manual price entries: %w", err)
	}
	manualSet := make(map[string]struct{}, len(manualModels))
	for _, model := range manualModels {
		if model = strings.TrimSpace(model); model != "" {
			manualSet[model] = struct{}{}
		}
	}
	filteredVariants := make([]*domain.PriceVariant, 0, len(normalizedVariants))
	variantModels := make([]string, 0, len(normalizedVariants))
	for _, variant := range normalizedVariants {
		model := variant.Model
		if _, manual := manualSet[model]; manual {
			continue
		}
		filteredVariants = append(filteredVariants, variant)
		variantModels = append(variantModels, model)
	}

	n, err := tr.PriceEntries.UpsertFromLiteLLM(ctx, orderedEntries)
	if err != nil {
		return 0, 0, err
	}
	nVar, err := tr.PriceVariants.UpsertFromLiteLLM(ctx, filteredVariants)
	if err != nil {
		return 0, 0, err
	}
	if _, err := tr.ReconcileLiteLLMSnapshot(ctx, models, variantModels); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return n, nVar, nil
}
