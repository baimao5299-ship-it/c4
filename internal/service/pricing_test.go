// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
)

func newPricingSvc(t *testing.T, fs *fakeStore) *Service {
	t.Helper()
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	return svc
}

type fakePriceFetcher struct {
	res *pricing.FetchResult
	err error
	url string
}

// serialPriceFetcher blocks its first call until released and records the
// maximum number of concurrent fetches. SyncPricingNow must hold the pricing
// mutation guard across fetch and persistence, so a second caller cannot enter
// the fetch while the first call is still in flight.
type serialPriceFetcher struct {
	started chan struct{}
	release chan struct{}
	active  atomic.Int32
	max     atomic.Int32
	calls   atomic.Int32
}

func (f *serialPriceFetcher) Fetch(ctx context.Context, _ string) (*pricing.FetchResult, error) {
	f.calls.Add(1)
	active := f.active.Add(1)
	for {
		old := f.max.Load()
		if active <= old || f.max.CompareAndSwap(old, active) {
			break
		}
	}
	defer f.active.Add(-1)
	select {
	case f.started <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &pricing.FetchResult{PriceEntries: []*domain.PriceEntry{{
		Model: "serial-model", Mode: domain.PriceModeToken,
		InputPerM: int64Ptr(100), OutputPerM: int64Ptr(200), Source: domain.PricingSourceLitellm,
	}}}, nil
}

func (f *fakePriceFetcher) Fetch(ctx context.Context, sourceURL string) (*pricing.FetchResult, error) {
	f.url = sourceURL
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func int64Ptr(v int64) *int64 { return &v }

type fakePricingSnapshotStore struct {
	*fakeStore
	calls         int
	models        []string
	variantModels []string
}

func (f *fakePricingSnapshotStore) ReconcileLiteLLMSnapshot(_ context.Context, models, variantModels []string) (int, error) {
	f.calls++
	f.models = append([]string(nil), models...)
	f.variantModels = append([]string(nil), variantModels...)
	return 0, nil
}

func TestSyncPricingUsesSourceModelsForSnapshotCompleteness(t *testing.T) {
	store := &fakePricingSnapshotStore{fakeStore: newFakeStore()}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	_, err := store.SetSetting(context.Background(), "price_source_url", domain.SettingTypeString, "http://example.com/prices.json")
	require.NoError(t, err)
	svc.SetPriceFetcher(&fakePriceFetcher{res: &pricing.FetchResult{
		Models:       []string{"metadata-only", "priced"},
		PriceEntries: []*domain.PriceEntry{{Model: "priced", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100), OutputPerM: int64Ptr(200), Source: domain.PricingSourceLitellm}},
		Skipped:      1,
	}})

	_, err = svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, store.calls)
	require.Equal(t, []string{"metadata-only", "priced"}, store.models)
}

func TestSyncPricingReconcilesOnlyNonManualVariantModels(t *testing.T) {
	store := &fakePricingSnapshotStore{fakeStore: newFakeStore()}
	store.priceEntries["manual"] = &domain.PriceEntry{
		Model: "manual", Mode: domain.PriceModeToken, Source: domain.PricingSourceManual,
		InputPerM: int64Ptr(100), OutputPerM: int64Ptr(200),
	}
	_, err := store.SetSetting(context.Background(), "price_source_url", domain.SettingTypeString, "http://example.com/prices.json")
	require.NoError(t, err)
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	svc.SetPriceFetcher(&fakePriceFetcher{res: &pricing.FetchResult{
		Models: []string{"manual", "automatic"},
		PriceEntries: []*domain.PriceEntry{
			{Model: "manual", Mode: domain.PriceModeToken, Source: domain.PricingSourceLitellm, InputPerM: int64Ptr(300), OutputPerM: int64Ptr(400)},
			{Model: "automatic", Mode: domain.PriceModeToken, Source: domain.PricingSourceLitellm, InputPerM: int64Ptr(500), OutputPerM: int64Ptr(600)},
		},
		Variants: []*domain.PriceVariant{{Model: "manual", Seq: 1}, {Model: "automatic", Seq: 1}},
	}})

	_, err = svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"automatic"}, store.variantModels,
		"manual models filtered from variant writes must also be absent from reconciliation")
}

func TestSyncPricingSerializesFetchAndPreventsOverlap(t *testing.T) {
	store := newFakeStore()
	_, err := store.SetSetting(context.Background(), "price_source_url", domain.SettingTypeString, "http://example.com/prices.json")
	require.NoError(t, err)
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)
	fetcher := &serialPriceFetcher{started: make(chan struct{}, 2), release: make(chan struct{})}
	svc.SetPriceFetcher(fetcher)

	firstDone := make(chan error, 1)
	go func() {
		_, callErr := svc.SyncPricingNow(context.Background())
		firstDone <- callErr
	}()
	select {
	case <-fetcher.started:
	case <-time.After(time.Second):
		t.Fatal("first pricing fetch did not start")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, callErr := svc.SyncPricingNow(context.Background())
		secondDone <- callErr
	}()
	// Give the second caller a scheduling opportunity while the first fetch is
	// blocked. It must remain outside the fetch, proving the lock spans I/O.
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, int32(1), fetcher.max.Load())
	close(fetcher.release)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	require.Equal(t, int32(2), fetcher.calls.Load())
}

func TestSyncPricingRejectsNilFetcherResult(t *testing.T) {
	store := newFakeStore()
	_, err := store.SetSetting(context.Background(), "price_source_url", domain.SettingTypeString, "http://example.com/prices.json")
	require.NoError(t, err)
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)
	svc.SetPriceFetcher(&fakePriceFetcher{})
	// A nil result with no error is malformed fetcher behavior; it must become a
	// controlled pricing error rather than panic while dereferencing PriceEntries.
	_, err = svc.SyncPricingNow(context.Background())
	require.ErrorIs(t, err, ErrPriceFetch)
	require.Contains(t, err.Error(), "nil result")
}

func TestPreviewPricingRejectsNilFetcherResult(t *testing.T) {
	store := newFakeStore()
	_, err := store.SetSetting(context.Background(), "price_source_url", domain.SettingTypeString, "http://example.com/prices.json")
	require.NoError(t, err)
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)
	svc.SetPriceFetcher(&fakePriceFetcher{})
	_, err = svc.PreviewPricingSync(context.Background())
	require.ErrorIs(t, err, ErrPriceFetch)
	require.Contains(t, err.Error(), "nil result")
}

func TestSyncPricingProtectsExplicitEmptySource(t *testing.T) {
	store := &fakePricingSnapshotStore{fakeStore: newFakeStore()}
	svc := New(store, nil, NopInvalidator{}, nil, nil, nil, nil)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	_, err := store.SetSetting(context.Background(), "price_source_url", domain.SettingTypeString, "http://example.com/prices.json")
	require.NoError(t, err)
	svc.SetPriceFetcher(&fakePriceFetcher{res: &pricing.FetchResult{Models: []string{}}})

	_, err = svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	require.Zero(t, store.calls, "an empty source must not prune the existing catalogue")
}

func TestPriceEntrySnapshotLoad(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "gpt-4o", Mode: domain.PriceModeToken, InputPerM: int64Ptr(250000), OutputPerM: int64Ptr(1000000), Source: domain.PricingSourceLitellm},
		{Model: "img-m", Mode: domain.PriceModeImage, PricePerImage: int64Ptr(5400), Source: domain.PricingSourceLitellm},
	})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	pe, err := svc.GetPriceEntry(context.Background(), "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(250000), *pe.InputPerM)
	_, err = svc.GetPriceEntry(context.Background(), "missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPricingSnapshotNormalizesModelWhitespace(t *testing.T) {
	fs := newFakeStore()
	model := "  gpt-spaced  "
	inPrice, outPrice, variantInput := int64(100000), int64(250000), int64(50000)
	fs.priceEntries[model] = &domain.PriceEntry{
		Model: model, Mode: domain.PriceModeToken,
		InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceManual,
	}
	fs.priceVariants[model] = []*domain.PriceVariant{{
		Model: model, Seq: 1, SetInputPerM: &variantInput,
	}}

	svc := newPricingSvc(t, fs)

	// Runtime billing accepts the canonical name and ignores accidental request
	// padding while retaining case-sensitive exact matching.
	rp, ok := svc.ResolvePrices("  gpt-spaced  ", 0, "", time.Now())
	require.True(t, ok)
	require.Equal(t, variantInput, *rp.InputPerM)
	require.Equal(t, outPrice, *rp.OutputPerM)
	_, ok = svc.ResolvePrices("GPT-SPACED", 0, "", time.Now())
	require.False(t, ok, "model matching remains case-sensitive")

	entries, err := svc.PriceEntriesForModels(context.Background(), []string{" gpt-spaced "})
	require.NoError(t, err)
	require.Contains(t, entries, "gpt-spaced")
	require.Equal(t, model, entries["gpt-spaced"].Model, "catalogue keeps the persisted model text")

	resolved, err := svc.ResolvedPricesForModels(context.Background(), []string{"gpt-spaced"}, "", 0, time.Now())
	require.NoError(t, err)
	require.Contains(t, resolved, "gpt-spaced")
	require.Equal(t, variantInput, *resolved["gpt-spaced"].InputPerM)
}

func TestPriceEntriesForModelsStartupFallbackNormalizesModelWhitespace(t *testing.T) {
	fs := newFakeStore()
	model := "  startup-spaced  "
	inPrice, outPrice := int64(100), int64(200)
	fs.priceEntries[model] = &domain.PriceEntry{
		Model: model, Mode: domain.PriceModeToken,
		InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceManual,
	}

	// Do not initialize the snapshot: this exercises the database fallback.
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil)
	got, err := svc.PriceEntriesForModels(context.Background(), []string{" startup-spaced "})
	require.NoError(t, err)
	require.Contains(t, got, "startup-spaced")
	require.Equal(t, inPrice, *got["startup-spaced"].InputPerM)
}

func TestPreviewPricingSyncNormalizesModelWhitespace(t *testing.T) {
	fs := newFakeStore()
	model := "preview-spaced"
	inPrice, outPrice := int64(100), int64(200)
	fs.priceEntries[model] = &domain.PriceEntry{
		Model: model, Mode: domain.PriceModeToken,
		InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceLitellm,
	}
	svc := newPricingSvc(t, fs)
	_, err := fs.SetSetting(context.Background(), "price_source_url", domain.SettingTypeString, "http://example.com/prices.json")
	require.NoError(t, err)
	svc.SetPriceFetcher(&fakePriceFetcher{res: &pricing.FetchResult{
		PriceEntries: []*domain.PriceEntry{{
			Model: "  " + model + "  ", Mode: domain.PriceModeToken,
			InputPerM: int64Ptr(300), OutputPerM: int64Ptr(400), Source: domain.PricingSourceLitellm,
		}},
	}})

	preview, err := svc.PreviewPricingSync(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, preview.ToUpdate, "padded upstream model matches the persisted row")
	require.Equal(t, 0, preview.ToAdd)
}

func TestPriceEntriesForModelsPagesStartupFallback(t *testing.T) {
	fs := newFakeStore()
	rows := make([]*domain.PriceEntry, 0, pricingReloadPage+1)
	for i := 0; i <= pricingReloadPage; i++ {
		rows = append(rows, &domain.PriceEntry{
			Model: fmt.Sprintf("model-%04d", i), Mode: domain.PriceModeToken,
			InputPerM: int64Ptr(int64(i + 1)), OutputPerM: int64Ptr(int64(i + 2)), Source: domain.PricingSourceLitellm,
		})
	}
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), rows)
	require.NoError(t, err)

	// Do not initialize the snapshot: this exercises the startup fallback.
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil)
	got, err := svc.PriceEntriesForModels(context.Background(), []string{"model-1000"})
	require.NoError(t, err)
	require.Contains(t, got, "model-1000")
	require.Equal(t, int64(1001), *got["model-1000"].InputPerM)
}

func TestPriceEntryManualValidation(t *testing.T) {
	fs := newFakeStore()
	svc := newPricingSvc(t, fs)
	_, err := svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{Model: "", Mode: domain.PriceModeToken})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100)})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100), OutputPerM: int64Ptr(200)})
	require.NoError(t, err)
}

func TestResolvePricesWithVariant(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000), Source: domain.PricingSourceManual},
	})
	require.NoError(t, err)
	_, err = fs.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{
		{Model: "m", Seq: 1, ServiceTier: strPtr("priority"), SetInputPerM: int64Ptr(150000)},
	})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	rp, ok := svc.ResolvePrices("m", 0, "priority", time.Now())
	require.True(t, ok)
	require.Equal(t, int64(150000), *rp.InputPerM)
}

func TestPricingSnapshotExcludesModelWhenAnyConditionalVariantIsInexact(t *testing.T) {
	tests := []struct {
		name    string
		variant *domain.PriceVariant
		tier    string
		tokens  int64
	}{
		{
			name:    "fast tier",
			variant: &domain.PriceVariant{Seq: 1, ServiceTier: strPtr("fast"), MultBP: intPtr(10)},
			tier:    "fast",
		},
		{
			name:    "large context",
			variant: &domain.PriceVariant{Seq: 1, CtxMin: int64Ptr(100_000), MultBP: intPtr(10)},
			tokens:  100_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			model := "inexact-" + strings.ReplaceAll(tt.name, " ", "-")
			_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{{
				Model: model, Mode: domain.PriceModeToken,
				InputPerM: int64Ptr(100), OutputPerM: int64Ptr(500), Source: domain.PricingSourceLitellm,
			}})
			require.NoError(t, err)
			tt.variant.Model = model
			_, err = fs.ReplacePriceVariants(context.Background(), model, []*domain.PriceVariant{tt.variant})
			require.NoError(t, err)

			svc := newPricingSvc(t, fs)
			_, ok := svc.ResolvePrices(model, 0, "", time.Now())
			require.False(t, ok, "an invalid conditional branch must fail the request precheck even when it does not match")
			_, ok = svc.ResolvePrices(model, tt.tokens, tt.tier, time.Now())
			require.False(t, ok, "the invalid conditional branch must not reach response-time billing")
		})
	}
}

func TestPricingSnapshotKeepsModelWhenAllConditionalVariantsAreExact(t *testing.T) {
	fs := newFakeStore()
	model := "exact-variants"
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{{
		Model: model, Mode: domain.PriceModeToken,
		InputPerM: int64Ptr(10_000), OutputPerM: int64Ptr(20_000), Source: domain.PricingSourceLitellm,
	}})
	require.NoError(t, err)
	_, err = fs.ReplacePriceVariants(context.Background(), model, []*domain.PriceVariant{
		{Model: model, Seq: 1, ServiceTier: strPtr("fast"), MultBP: intPtr(10)},
		{Model: model, Seq: 2, CtxMin: int64Ptr(100_000), MultBP: intPtr(10)},
	})
	require.NoError(t, err)

	svc := newPricingSvc(t, fs)
	_, ok := svc.ResolvePrices(model, 0, "", time.Now())
	require.True(t, ok)
	_, ok = svc.ResolvePrices(model, 0, "fast", time.Now())
	require.True(t, ok)
	_, ok = svc.ResolvePrices(model, 100_000, "", time.Now())
	require.True(t, ok)
}

func TestReplacePriceVariants_MultBPValidation(t *testing.T) {
	fs := newFakeStore()
	svc := newPricingSvc(t, fs)
	// MaxInt rejected
	maxInt := 1 << 30
	_, err := svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &maxInt}})
	require.ErrorIs(t, err, ErrInvalidInput)
	// negative rejected
	neg := -1
	_, err = svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &neg}})
	require.ErrorIs(t, err, ErrInvalidInput)
	// boundary 100000 allowed, 100001 rejected
	v100k := 100000
	_, err = svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &v100k}})
	require.NoError(t, err)
	v100001 := 100001
	_, err = svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &v100001}})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestPriceVariantMultiplierRejectsPrecisionChangingCombinations(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{{
		Model: "tiny", Mode: domain.PriceModeToken,
		InputPerM: int64Ptr(100), OutputPerM: int64Ptr(500), Source: domain.PricingSourceManual,
	}})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	mult := 10 // x0.001: 100 -> 0.1 and 500 -> 0.5 internal units
	_, err = svc.ReplacePriceVariants(context.Background(), "tiny", []*domain.PriceVariant{{Model: "tiny", Seq: 1, MultBP: &mult}})
	require.ErrorIs(t, err, ErrInvalidInput)

	exactInput, exactOutput := int64(10_000), int64(20_000)
	_, err = svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{
		Model: "exact", Mode: domain.PriceModeToken, InputPerM: &exactInput, OutputPerM: &exactOutput,
	})
	require.NoError(t, err)
	_, err = svc.ReplacePriceVariants(context.Background(), "exact", []*domain.PriceVariant{{Model: "exact", Seq: 1, MultBP: &mult}})
	require.NoError(t, err)
}

func TestPriceEntryUpdateCannotInvalidateExistingVariantPrecision(t *testing.T) {
	fs := newFakeStore()
	baseInput, baseOutput, mult := int64(10_000), int64(20_000), 10
	_, err := fs.UpsertPriceEntryManual(context.Background(), &repository.PriceEntryManual{
		Model: "changing", Mode: domain.PriceModeToken, InputPerM: &baseInput, OutputPerM: &baseOutput,
	})
	require.NoError(t, err)
	_, err = fs.ReplacePriceVariants(context.Background(), "changing", []*domain.PriceVariant{{Model: "changing", Seq: 1, MultBP: &mult}})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)

	inexactInput, inexactOutput := int64(100), int64(500)
	_, err = svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{
		Model: "changing", Mode: domain.PriceModeToken, InputPerM: &inexactInput, OutputPerM: &inexactOutput,
	})
	require.ErrorIs(t, err, ErrInvalidInput)
	stored, getErr := fs.GetPriceEntry(context.Background(), "changing")
	require.NoError(t, getErr)
	require.Equal(t, baseInput, *stored.InputPerM, "a rejected update must leave the previous price intact")
}

func TestReplacePriceVariants_CallSetPricePerCall(t *testing.T) {
	fs := newFakeStore()
	svc := newPricingSvc(t, fs)
	callPrice := int64(2000)
	_, err := svc.ReplacePriceVariants(context.Background(), "m-call", []*domain.PriceVariant{{Model: "m-call", Seq: 1, SetPricePerCall: &callPrice}})
	require.NoError(t, err)
	// also resolver yields that price
	_, err = fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "m-call", Mode: domain.PriceModeCall, PricePerCall: int64Ptr(1000), Source: domain.PricingSourceManual},
	})
	require.NoError(t, err)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	rp, ok := svc.ResolvePrices("m-call", 0, "auto", time.Now())
	require.True(t, ok)
	require.NotNil(t, rp.PricePerCall)
	require.Equal(t, callPrice, *rp.PricePerCall)
	// empty effect should be rejected
	_, err = svc.ReplacePriceVariants(context.Background(), "m-call", []*domain.PriceVariant{{Model: "m-call", Seq: 2}})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestSyncPricingGuardsManualVariants(t *testing.T) {
	fs := newFakeStore()
	// create manual entry + custom variants for model X
	_, err := fs.UpsertPriceEntryManual(context.Background(), &repository.PriceEntryManual{Model: "guard-model", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000)})
	require.NoError(t, err)
	mult := 5000
	_, err = fs.ReplacePriceVariants(context.Background(), "guard-model", []*domain.PriceVariant{{Model: "guard-model", Seq: 99, MultBP: &mult}})
	require.NoError(t, err)
	// also create a litellm model that should be overwritten
	_, err = fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{{Model: "litellm-model", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000), Source: domain.PricingSourceLitellm}})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	// settings needed for SyncPricingNow
	_, err = fs.SetSetting(context.Background(), "price_source_url", domain.SettingTypeString, "http://example.com/prices.json")
	require.NoError(t, err)
	// fake fetcher emits variants for both models: guard-model's variants should be dropped, litellm-model's should be applied
	fetcher := &fakePriceFetcher{res: &pricing.FetchResult{
		PriceEntries: []*domain.PriceEntry{{Model: "guard-model", Mode: domain.PriceModeToken, InputPerM: int64Ptr(999), OutputPerM: int64Ptr(999), Source: domain.PricingSourceLitellm}},
		Variants: []*domain.PriceVariant{
			{Model: "guard-model", Seq: 1, MultBP: intPtr(20000)},
			{Model: "litellm-model", Seq: 1, MultBP: intPtr(30000)},
		},
	}}
	svc.SetPriceFetcher(fetcher)
	_, err = svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	// guard-model variants must survive (seq 99), not replaced by fetcher's seq 1
	vars, err := fs.ListPriceVariants(context.Background(), "guard-model")
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, 99, vars[0].Seq)
	require.Equal(t, 5000, *vars[0].MultBP)
	// litellm-model variants should be applied
	vars2, err := fs.ListPriceVariants(context.Background(), "litellm-model")
	require.NoError(t, err)
	require.Len(t, vars2, 1)
	require.Equal(t, 1, vars2[0].Seq)
	require.Equal(t, 30000, *vars2[0].MultBP)
	// entry for guard-model must remain manual (not overwritten)
	pe, err := fs.GetPriceEntry(context.Background(), "guard-model")
	require.NoError(t, err)
	require.Equal(t, domain.PricingSourceManual, pe.Source)
}
