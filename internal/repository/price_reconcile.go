// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/is7qin/c3api/internal/ent/priceentry"
	"github.com/is7qin/c3api/internal/ent/pricevariant"
)

var ErrSuspiciousPriceSnapshot = errors.New("pricing snapshot is unexpectedly smaller than the current catalogue")

const snapshotShrinkGuardMinExisting = 100
const snapshotOverlapGuardMinExisting = 100
const snapshotOverlapMinPercent = 5

// ReconcileLiteLLMSnapshot removes official rows that disappeared from a
// complete LiteLLM snapshot and clears their conditional variants. It also
// clears variants for a current official model when the new snapshot contains
// no variants for it. Manual rows are never touched: an administrator-owned
// model may intentionally be absent from the official feed.
//
// The caller must only invoke this after a complete, successfully parsed
// snapshot. Empty or partially skipped fetches are deliberately left alone by
// the worker/service guard, so a transient upstream response cannot wipe the
// catalogue.
func (r *Repository) ReconcileLiteLLMSnapshot(ctx context.Context, models, variantModels []string) (int, error) {
	models = normalizeModelList(models)
	if len(models) == 0 {
		return 0, nil
	}
	variantModels = normalizeModelList(variantModels)
	variantSet := make(map[string]struct{}, len(variantModels))
	for _, model := range variantModels {
		variantSet[model] = struct{}{}
	}

	tx, err := r.Client.Tx(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // nolint:errcheck // Commit closes the transaction.
	currentEntries, err := tx.PriceEntry.Query().
		Where(priceentry.SourceEQ(priceentry.SourceLitellm)).
		All(ctx)
	if err != nil {
		return 0, err
	}
	currentRows := len(currentEntries)
	currentModels := make([]string, 0, currentRows)
	for _, row := range currentEntries {
		if row != nil {
			if model := strings.TrimSpace(row.Model); model != "" {
				currentModels = append(currentModels, model)
			}
		}
	}
	manualCandidates, err := tx.PriceEntry.Query().
		Where(priceentry.SourceEQ(priceentry.SourceManual), priceentry.ModelIn(models...)).
		All(ctx)
	if err != nil {
		return 0, err
	}
	manualSet := make(map[string]struct{}, len(manualCandidates))
	for _, row := range manualCandidates {
		if row != nil {
			if model := strings.TrimSpace(row.Model); model != "" {
				manualSet[model] = struct{}{}
			}
		}
	}
	candidateRows := len(models) - len(manualSet)
	candidateOfficialModels := make([]string, 0, candidateRows)
	for _, model := range models {
		if _, manual := manualSet[model]; !manual {
			candidateOfficialModels = append(candidateOfficialModels, model)
		}
	}
	if suspiciousPriceSnapshotShrink(currentRows, candidateRows) || suspiciousPriceSnapshotContent(currentModels, candidateOfficialModels) {
		overlap := snapshotModelOverlap(currentModels, candidateOfficialModels)
		return 0, fmt.Errorf("%w: current=%d candidate=%d overlap=%d", ErrSuspiciousPriceSnapshot, currentRows, candidateRows, overlap)
	}

	// Discover stale official rows inside the same transaction used for the
	// deletes. The source predicate protects manual rows even if a model name
	// appears in an old or malformed feed.
	rows, err := tx.PriceEntry.Query().
		Where(priceentry.SourceEQ(priceentry.SourceLitellm), priceentry.ModelNotIn(models...)).
		All(ctx)
	if err != nil {
		return 0, err
	}
	staleModels := make([]string, 0, len(rows))
	for _, row := range rows {
		if row != nil && strings.TrimSpace(row.Model) != "" {
			staleModels = append(staleModels, row.Model)
		}
	}
	removed := 0
	if len(staleModels) > 0 {
		if n, err := tx.PriceVariant.Delete().Where(pricevariant.ModelIn(staleModels...)).Exec(ctx); err != nil {
			return 0, err
		} else {
			removed += n
		}
		if n, err := tx.PriceEntry.Delete().
			Where(priceentry.SourceEQ(priceentry.SourceLitellm), priceentry.ModelIn(staleModels...)).
			Exec(ctx); err != nil {
			return 0, err
		} else {
			removed += n
		}
	}

	// A missing variant list is meaningful for a complete snapshot: remove old
	// official variants so a previous tier cannot silently keep changing billing.
	// Query manual rows first and exclude them from the clear set.
	clearModels := make([]string, 0, len(models))
	for _, model := range models {
		if _, has := variantSet[model]; !has {
			clearModels = append(clearModels, model)
		}
	}
	if len(clearModels) > 0 {
		officialClear := make([]string, 0, len(clearModels))
		for _, model := range clearModels {
			if _, manual := manualSet[model]; !manual {
				officialClear = append(officialClear, model)
			}
		}
		if len(officialClear) > 0 {
			if n, err := tx.PriceVariant.Delete().Where(pricevariant.ModelIn(officialClear...)).Exec(ctx); err != nil {
				return 0, err
			} else {
				removed += n
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}

func suspiciousPriceSnapshotShrink(current, candidate int) bool {
	if current < snapshotShrinkGuardMinExisting {
		return false
	}
	// Use ceil(current/2) without multiplying candidate, avoiding overflow and
	// treating an exact 50%% snapshot as the conservative acceptance boundary.
	return candidate < (current+1)/2
}

// suspiciousPriceSnapshotContent catches a complete feed that has roughly the
// same size as the current catalogue but contains almost none of the same
// model identifiers. Such a response is more likely an HTML/error document or
// a provider namespace change than a legitimate price removal. Small custom
// catalogues remain allowed, matching the count guard's compatibility rule.
func suspiciousPriceSnapshotContent(current, candidate []string) bool {
	current = normalizeModelList(current)
	candidate = normalizeModelList(candidate)
	if len(current) < snapshotOverlapGuardMinExisting || len(candidate) == 0 {
		return false
	}
	overlap := snapshotModelOverlap(current, candidate)
	denom := len(current)
	if len(candidate) < denom {
		denom = len(candidate)
	}
	return overlap*100 < denom*snapshotOverlapMinPercent
}

func snapshotModelOverlap(current, candidate []string) int {
	if len(current) == 0 || len(candidate) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(current))
	for _, model := range current {
		set[model] = struct{}{}
	}
	overlap := 0
	seen := make(map[string]struct{}, len(candidate))
	for _, model := range candidate {
		if _, duplicate := seen[model]; duplicate {
			continue
		}
		seen[model] = struct{}{}
		if _, ok := set[model]; ok {
			overlap++
		}
	}
	return overlap
}

func normalizeModelList(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}
