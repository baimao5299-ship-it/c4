// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/priceentry"
	"github.com/is7qin/c3api/internal/repository"
)

func TestPGApplyLiteLLMSnapshotIsAtomicAndGuardsAbruptShrink(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	price := func(model string, value int64) *domain.PriceEntry {
		return &domain.PriceEntry{Model: model, Mode: domain.PriceModeToken, InputPerM: &value, Source: domain.PricingSourceLitellm}
	}

	initial := make([]*domain.PriceEntry, 0, 100)
	models := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		model := fmt.Sprintf("model-%03d", i)
		initial = append(initial, price(model, int64(100+i)))
		models = append(models, model)
	}
	n, nVar, err := repos.ApplyLiteLLMSnapshot(ctx, initial, nil, models)
	require.NoError(t, err)
	require.Equal(t, 100, n)
	require.Zero(t, nVar)

	_, _, err = repos.ApplyLiteLLMSnapshot(ctx,
		[]*domain.PriceEntry{price("model-000", 999)}, nil, []string{"model-000"})
	require.ErrorIs(t, err, repository.ErrSuspiciousPriceSnapshot)
	count, countErr := repos.Client.PriceEntry.Query().Where(priceentry.SourceEQ(priceentry.SourceLitellm)).Count(ctx)
	require.NoError(t, countErr)
	require.Equal(t, 100, count)
	row, getErr := repos.GetPriceEntry(ctx, "model-000")
	require.NoError(t, getErr)
	require.Equal(t, int64(100), *row.InputPerM, "shrink rejection rolls back the preceding base upsert")

	badVariant := &domain.PriceVariant{Model: "model-000", Seq: 1}
	_, _, err = repos.ApplyLiteLLMSnapshot(ctx,
		[]*domain.PriceEntry{price("model-000", 888)}, []*domain.PriceVariant{badVariant}, models)
	require.Error(t, err)
	row, getErr = repos.GetPriceEntry(ctx, "model-000")
	require.NoError(t, getErr)
	require.Equal(t, int64(100), *row.InputPerM, "a variant failure rolls back base price writes")
}
