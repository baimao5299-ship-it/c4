// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

var ErrPriceFetch = errors.New("service: price fetch failed")

type PricingSyncStats struct {
	Rows     int
	Skipped  int
	Updated  int
	Variants int
}

type PricingPreview struct {
	ToAdd           int                   `json:"to_add"`
	ToUpdate        int                   `json:"to_update"`
	Skipped         int                   `json:"skipped"`
	Entries         []PricingPreviewEntry `json:"entries"`
	VariantsChanged int                   `json:"variants_changed"`
}

type PricingPreviewEntry struct {
	Model  string `json:"model"`
	Mode   string `json:"mode"`
	Action string `json:"action"` // add/update
}

type priceSnapshot struct {
	// entries contains only models whose conditional branches can be represented
	// exactly on the billing price grid. It is the runtime/billing view.
	entries map[string]*domain.PriceEntry
	// catalogue keeps every persisted row, including a row temporarily excluded
	// from runtime billing because one conditional variant is invalid. The user
	// model monitor can therefore show the authoritative catalogue price instead
	// of silently dropping a model.
	catalogue map[string]*domain.PriceEntry
	variants  map[string][]*domain.PriceVariant
	// Sorted keys are built once with the immutable snapshot. Alias resolution
	// is used by both billing and projections, so rebuilding them per request
	// would add avoidable hot-path work and could make paths disagree.
	entryKeys     []string
	catalogueKeys []string
}

const pricingReloadPage = 1000

func (s *Service) ReloadPricing() {
	s.pricingMutationMu.Lock()
	defer s.pricingMutationMu.Unlock()
	s.reloadPricingUnlocked(context.Background())
}

// ReloadPricingForMutation refreshes the snapshot for a caller that already
// holds WithPricingMutation. It is intentionally narrow: the pricing sync
// worker uses it while its repository writes and snapshot publication are
// covered by the same service lock.
func (s *Service) ReloadPricingForMutation() { s.reloadPricingUnlocked(context.Background()) }

// ReloadPricingCtxForMutation is the context-aware counterpart used by code
// that already owns the mutation guard and must preserve its cancellation
// budget while rebuilding the immutable snapshot.
func (s *Service) ReloadPricingCtxForMutation(ctx context.Context) error {
	return s.reloadPricingCtxUnlocked(ctx)
}

func (s *Service) ReloadPricingCtx(ctx context.Context) error {
	s.pricingMutationMu.Lock()
	defer s.pricingMutationMu.Unlock()
	return s.reloadPricingCtxUnlocked(ctx)
}
func (s *Service) reloadPricingCtxUnlocked(ctx context.Context) error {
	m, err := s.loadPricingSnapshot(ctx)
	if err != nil {
		return err
	}
	s.priceSnapshot.Store(m)
	return nil
}
func (s *Service) reloadPricing(ctx context.Context) {
	s.pricingMutationMu.Lock()
	defer s.pricingMutationMu.Unlock()
	s.reloadPricingUnlocked(ctx)
}
func (s *Service) reloadPricingUnlocked(ctx context.Context) {
	m, err := s.loadPricingSnapshot(ctx)
	if err != nil {
		return
	}
	s.priceSnapshot.Store(m)
}
func (s *Service) loadPricingSnapshot(ctx context.Context) (*priceSnapshot, error) {
	var all []*domain.PriceEntry
	for offset := 0; ; offset += pricingReloadPage {
		rows, _, err := s.store.ListPriceEntries(ctx, repository.ListQuery{Limit: pricingReloadPage, Offset: offset, Sort: "model", Order: "asc"}, nil, nil, nil, "")
		if err != nil {
			if s.log != nil {
				s.log.Warn("pricing snapshot reload failed", logx.Error(err))
			}
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < pricingReloadPage {
			break
		}
	}
	variants, err := s.store.ListAllPriceVariants(ctx)
	if err != nil {
		if s.log != nil {
			s.log.Warn("pricing variant snapshot reload failed", logx.Error(err))
		}
		return nil, err
	}
	entriesMap := make(map[string]*domain.PriceEntry, len(all))
	catalogueMap := make(map[string]*domain.PriceEntry, len(all))
	for _, e := range all {
		if e == nil {
			continue
		}
		model := strings.TrimSpace(e.Model)
		if model == "" {
			continue
		}
		entriesMap[model] = e
		catalogueMap[model] = e
	}
	vMap := make(map[string][]*domain.PriceVariant)
	for _, v := range variants {
		if v == nil {
			continue
		}
		model := strings.TrimSpace(v.Model)
		if model == "" {
			continue
		}
		vMap[model] = append(vMap[model], v)
	}
	for _, lst := range vMap {
		sort.Slice(lst, func(i, j int) bool { return lst[i].Seq < lst[j].Seq })
	}
	// A model is only usable when every persisted conditional branch can be
	// represented on the same 1e-5 USD price grid. Runtime resolution cannot
	// know which tier/context branch a request will select, so retaining a
	// partially valid model would let a late no_price result silently bill zero.
	for model, entry := range entriesMap {
		valid := true
		for _, variant := range vMap[model] {
			if err := domain.ValidateVariantPricePrecision(entry, variant); err != nil {
				valid = false
				if s.log != nil {
					s.log.Warn("pricing: excluding model with unrepresentable variant",
						logx.String("model", model), logx.Int("variant_seq", variant.Seq), logx.Error(err))
				}
				break
			}
		}
		if !valid {
			delete(entriesMap, model)
			delete(vMap, model)
		}
	}
	return &priceSnapshot{
		entries:       entriesMap,
		catalogue:     catalogueMap,
		variants:      vMap,
		entryKeys:     sortedModelKeys(entriesMap),
		catalogueKeys: sortedModelKeys(catalogueMap),
	}, nil
}

// ResolvePrices 模型价格解析：快照零 DB 读 + 委托 domain 解析核
// （entry→基底→首中变体；纯函数与测试假实现共用，防逻辑漂移）。
func (s *Service) ResolvePrices(model string, promptTokens int64, tier string, at time.Time) (domain.ResolvedPrices, bool) {
	snap := s.priceSnapshot.Load()
	if snap == nil {
		return domain.ResolvedPrices{}, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return domain.ResolvedPrices{}, false
	}
	if s.tzLoc != nil {
		at = at.In(s.tzLoc)
	}
	entryKeys := (*snap).entryKeys
	if len(entryKeys) == 0 && len((*snap).entries) > 0 {
		entryKeys = sortedModelKeys((*snap).entries)
	}
	key, ok := priceLookupKey(entryKeys, model, (*snap).entries, (*snap).variants)
	if !ok {
		return domain.ResolvedPrices{}, false
	}
	return domain.ResolveEntryPrices((*snap).entries[key], (*snap).variants[key], tier, promptTokens, at)
}

// PriceEntriesForModels returns the current immutable catalogue rows for the
// requested model names. It intentionally reads the catalogue view rather than
// the runtime billing view, so a model with an invalid conditional branch can
// still explain its official price in the monitor. The user-facing channel
// monitor calls this on every refresh, so it must stay off the database hot path
// after startup and after a pricing reload. A database fallback keeps the
// endpoint useful during the short window before the first snapshot is built.
func (s *Service) PriceEntriesForModels(ctx context.Context, models []string) (map[string]*domain.PriceEntry, error) {
	wanted := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model != "" {
			wanted[model] = struct{}{}
		}
	}
	out := make(map[string]*domain.PriceEntry, len(wanted))
	if len(wanted) == 0 {
		return out, nil
	}
	if snap := s.priceSnapshot.Load(); snap != nil {
		catalogueKeys := snap.catalogueKeys
		if len(catalogueKeys) == 0 && len(snap.catalogue) > 0 {
			catalogueKeys = sortedModelKeys(snap.catalogue)
		}
		for model := range wanted {
			if key, ok := priceLookupKey(catalogueKeys, model, snap.catalogue, nil); ok {
				if entry := snap.catalogue[key]; entry != nil {
					out[model] = entry
				}
			}
		}
		return out, nil
	}
	// Startup fallback: load the complete catalogue before resolving aliases.
	// Alias ambiguity cannot be known from a single page (for example two
	// provider-qualified rows may be on different pages), so stopping after the
	// first exact hit could expose the wrong provider price.
	allCatalogue := make(map[string]*domain.PriceEntry)
	for offset := 0; ; offset += pricingReloadPage {
		rows, _, err := s.store.ListPriceEntries(ctx, repository.ListQuery{
			Limit: pricingReloadPage, Offset: offset, Sort: "model", Order: "asc",
		}, nil, nil, nil, "")
		if err != nil {
			return nil, err
		}
		for _, entry := range rows {
			if entry != nil {
				model := strings.TrimSpace(entry.Model)
				if model != "" {
					allCatalogue[model] = entry
				}
			}
		}
		if len(rows) < pricingReloadPage {
			break
		}
	}
	keys := sortedModelKeys(allCatalogue)
	for model := range wanted {
		if key, ok := priceLookupKey(keys, model, allCatalogue, nil); ok {
			if entry := allCatalogue[key]; entry != nil {
				out[model] = entry
			}
		}
	}
	return out, nil
}

// pricingProjectionForModels reads the catalogue and runtime views from one
// immutable snapshot. Keeping the two maps together prevents a pricing reload
// between separate calls from pairing an old official price with a new
// effective price in the user-facing model monitor.
func (s *Service) pricingProjectionForModels(ctx context.Context, models []string, tier string, promptTokens int64, at time.Time) (map[string]*domain.PriceEntry, map[string]domain.ResolvedPrices, error) {
	wanted := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model = strings.TrimSpace(model); model != "" {
			wanted[model] = struct{}{}
		}
	}
	catalogue := make(map[string]*domain.PriceEntry, len(wanted))
	resolved := make(map[string]domain.ResolvedPrices, len(wanted))
	if len(wanted) == 0 {
		return catalogue, resolved, nil
	}
	snap := s.priceSnapshot.Load()
	if snap == nil {
		loaded, err := s.loadPricingSnapshot(ctx)
		if err != nil {
			return nil, nil, err
		}
		snap = loaded
	}
	if s.tzLoc != nil {
		at = at.In(s.tzLoc)
	}
	catalogueKeys := snap.catalogueKeys
	if len(catalogueKeys) == 0 && len(snap.catalogue) > 0 {
		catalogueKeys = sortedModelKeys(snap.catalogue)
	}
	entryKeys := snap.entryKeys
	if len(entryKeys) == 0 && len(snap.entries) > 0 {
		entryKeys = sortedModelKeys(snap.entries)
	}
	for model := range wanted {
		if key, ok := priceLookupKey(catalogueKeys, model, snap.catalogue, nil); ok {
			if entry := snap.catalogue[key]; entry != nil {
				catalogue[model] = entry
			}
		}
		if key, ok := priceLookupKey(entryKeys, model, snap.entries, snap.variants); ok {
			if rp, ok := domain.ResolveEntryPrices(snap.entries[key], snap.variants[key], tier, promptTokens, at); ok {
				resolved[model] = rp
			}
		}
	}
	return catalogue, resolved, nil
}

// ResolvedPricesForModels returns the same current price projection used by
// billing. The normal user tier and a zero-token context are explicit because
// context-dependent variants can only be finalized once a real request exists.
func (s *Service) ResolvedPricesForModels(ctx context.Context, models []string, tier string, promptTokens int64, at time.Time) (map[string]domain.ResolvedPrices, error) {
	wanted := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model = strings.TrimSpace(model); model != "" {
			wanted[model] = struct{}{}
		}
	}
	out := make(map[string]domain.ResolvedPrices, len(wanted))
	if len(wanted) == 0 {
		return out, nil
	}
	snap := s.priceSnapshot.Load()
	if snap == nil {
		loaded, err := s.loadPricingSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		snap = loaded
	}
	if s.tzLoc != nil {
		at = at.In(s.tzLoc)
	}
	entryKeys := snap.entryKeys
	if len(entryKeys) == 0 && len(snap.entries) > 0 {
		entryKeys = sortedModelKeys(snap.entries)
	}
	for model := range wanted {
		if key, ok := priceLookupKey(entryKeys, model, (*snap).entries, (*snap).variants); ok {
			if resolved, ok := domain.ResolveEntryPrices((*snap).entries[key], (*snap).variants[key], tier, promptTokens, at); ok {
				out[model] = resolved
			}
		}
	}
	return out, nil
}

// validation helpers
func (s *Service) UpsertPriceEntry(ctx context.Context, m *repository.PriceEntryManual) (*domain.PriceEntry, error) {
	if m.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidInput)
	}
	if !m.Mode.Valid() {
		return nil, fmt.Errorf("%w: invalid mode %q", ErrInvalidInput, m.Mode)
	}
	switch m.Mode {
	case domain.PriceModeToken:
		if m.InputPerM == nil || m.OutputPerM == nil {
			return nil, fmt.Errorf("%w: token mode requires input_per_m+output_per_m", ErrInvalidInput)
		}
	case domain.PriceModeCall:
		if m.PricePerCall == nil {
			return nil, fmt.Errorf("%w: call mode requires price_per_call", ErrInvalidInput)
		}
	case domain.PriceModeImage:
		if m.ImgInTokPerM == nil && m.ImgOutTokPerM == nil && m.PricePerImage == nil {
			return nil, fmt.Errorf("%w: image mode requires at least one image component", ErrInvalidInput)
		}
	}
	nonNeg := func(v *int64, name string) error {
		if v != nil && *v < 0 {
			return fmt.Errorf("%w: %s must be >=0", ErrInvalidInput, name)
		}
		return nil
	}
	for _, f := range []struct {
		name string
		v    *int64
	}{
		{"input_per_m", m.InputPerM}, {"output_per_m", m.OutputPerM}, {"cache_read_per_m", m.CacheReadPerM}, {"cache_write_per_m", m.CacheWritePerM},
		{"price_per_call", m.PricePerCall}, {"img_in_tok_per_m", m.ImgInTokPerM}, {"img_out_tok_per_m", m.ImgOutTokPerM}, {"price_per_image", m.PricePerImage},
	} {
		if err := nonNeg(f.v, f.name); err != nil {
			return nil, err
		}
	}
	s.pricingMutationMu.Lock()
	defer s.pricingMutationMu.Unlock()
	candidate := &domain.PriceEntry{
		Model: m.Model, Mode: m.Mode,
		InputPerM: m.InputPerM, OutputPerM: m.OutputPerM,
		CacheReadPerM: m.CacheReadPerM, CacheWritePerM: m.CacheWritePerM,
		PricePerCall: m.PricePerCall,
		ImgInTokPerM: m.ImgInTokPerM, ImgOutTokPerM: m.ImgOutTokPerM,
		PricePerImage: m.PricePerImage,
	}
	variants, err := s.store.ListPriceVariants(ctx, m.Model)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	for _, v := range variants {
		if err := validateVariantPricePrecision(candidate, v); err != nil {
			return nil, err
		}
	}
	p, err := s.store.UpsertPriceEntryManual(ctx, m)
	if err != nil {
		return nil, err
	}
	s.reloadPricingUnlocked(ctx)
	return p, nil
}

func (s *Service) DeletePriceEntry(ctx context.Context, model string) error {
	s.pricingMutationMu.Lock()
	defer s.pricingMutationMu.Unlock()
	err := s.store.WithTx(ctx, func(tx repository.TxStore) error {
		if err := tx.DeletePriceVariantsByModel(ctx, model); err != nil {
			return err
		}
		if err := tx.DeletePriceEntryManual(ctx, model); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return mapRepoErr(err)
	}
	s.reloadPricingUnlocked(ctx)
	return nil
}

func (s *Service) GetPriceEntry(ctx context.Context, model string) (*domain.PriceEntry, error) {
	pe, err := s.store.GetPriceEntry(ctx, model)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return pe, nil
}

func (s *Service) ListPriceEntries(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, mode *domain.PriceMode, provider *string, model string) ([]*domain.PriceEntry, int64, error) {
	if source != nil && !source.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid source %q", ErrInvalidInput, *source)
	}
	if mode != nil && !mode.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid mode %q", ErrInvalidInput, *mode)
	}
	if err := validateListQuery(q, listSortFields["price_entries"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListPriceEntries(ctx, q, source, mode, provider, model)
}

func (s *Service) ListPriceVariants(ctx context.Context, model string) ([]*domain.PriceVariant, error) {
	return s.store.ListPriceVariants(ctx, model)
}

func (s *Service) ReplacePriceVariants(ctx context.Context, model string, variants []*domain.PriceVariant) ([]*domain.PriceVariant, error) {
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidInput)
	}
	seenSeq := make(map[int]struct{}, len(variants))
	for _, v := range variants {
		if err := validatePriceVariant(v, seenSeq); err != nil {
			return nil, err
		}
	}
	s.pricingMutationMu.Lock()
	defer s.pricingMutationMu.Unlock()
	entry, err := s.store.GetPriceEntry(ctx, model)
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			return nil, mapRepoErr(err)
		}
		entry = nil
	}
	for _, v := range variants {
		if err := validateVariantPricePrecision(entry, v); err != nil {
			return nil, err
		}
	}
	out, err := s.store.ReplacePriceVariants(ctx, model, variants)
	if err != nil {
		return nil, err
	}
	s.reloadPricingUnlocked(ctx)
	return out, nil
}

func validateVariantPricePrecision(entry *domain.PriceEntry, v *domain.PriceVariant) error {
	if err := domain.ValidateVariantPricePrecision(entry, v); err != nil {
		return fmt.Errorf("%w: variant seq %d: %v", ErrInvalidInput, v.Seq, err)
	}
	return nil
}

// validatePriceVariant keeps persisted conditional pricing deterministic. The
// repository constraint covers only the effect presence rule; the remaining
// bounds must be enforced before the replace transaction so invalid rows cannot
// be accepted by lightweight stores or direct database adapters.
func validatePriceVariant(v *domain.PriceVariant, seen map[int]struct{}) error {
	if v == nil {
		return fmt.Errorf("%w: variant must not be null", ErrInvalidInput)
	}
	if v.Seq < 1 {
		return fmt.Errorf("%w: variant seq must be >= 1", ErrInvalidInput)
	}
	if _, ok := seen[v.Seq]; ok {
		return fmt.Errorf("%w: duplicate variant seq %d", ErrInvalidInput, v.Seq)
	}
	seen[v.Seq] = struct{}{}
	if v.ServiceTier != nil && (strings.TrimSpace(*v.ServiceTier) == "" || len([]byte(*v.ServiceTier)) > 64) {
		return fmt.Errorf("%w: variant seq %d service_tier is invalid", ErrInvalidInput, v.Seq)
	}
	if v.CtxMin != nil && *v.CtxMin < 0 {
		return fmt.Errorf("%w: variant seq %d ctx_min must be >= 0", ErrInvalidInput, v.Seq)
	}
	if v.CtxMax != nil && *v.CtxMax < 0 {
		return fmt.Errorf("%w: variant seq %d ctx_max must be >= 0", ErrInvalidInput, v.Seq)
	}
	if v.CtxMin != nil && v.CtxMax != nil && *v.CtxMin >= *v.CtxMax {
		return fmt.Errorf("%w: variant seq %d ctx_min must be less than ctx_max", ErrInvalidInput, v.Seq)
	}
	if v.TimeStart != nil && !validVariantClock(*v.TimeStart) {
		return fmt.Errorf("%w: variant seq %d time_start must be HH:MM", ErrInvalidInput, v.Seq)
	}
	if v.TimeEnd != nil && !validVariantClock(*v.TimeEnd) {
		return fmt.Errorf("%w: variant seq %d time_end must be HH:MM", ErrInvalidInput, v.Seq)
	}
	if v.DowMask != nil && (*v.DowMask < 0 || *v.DowMask > 0x7f) {
		return fmt.Errorf("%w: variant seq %d dow_mask must use 7 bits", ErrInvalidInput, v.Seq)
	}
	if v.MultBP != nil && (*v.MultBP < 0 || *v.MultBP > 100000) {
		return fmt.Errorf("%w: variant seq %d multiplier must be in [0,10]", ErrInvalidInput, v.Seq)
	}
	for _, effect := range []struct {
		name  string
		value *int64
	}{
		{"set_input_per_m", v.SetInputPerM}, {"set_output_per_m", v.SetOutputPerM},
		{"set_cache_read_per_m", v.SetCacheReadPerM}, {"set_cache_creation_per_m", v.SetCacheCreationPerM},
		{"set_price_per_call", v.SetPricePerCall}, {"set_img_in_tok_per_m", v.SetImgInTokPerM},
		{"set_img_out_tok_per_m", v.SetImgOutTokPerM}, {"set_price_per_image", v.SetPricePerImage},
	} {
		if effect.value != nil && *effect.value < 0 {
			return fmt.Errorf("%w: variant seq %d %s must be >= 0", ErrInvalidInput, v.Seq, effect.name)
		}
	}
	if v.MultBP == nil && v.SetInputPerM == nil && v.SetOutputPerM == nil && v.SetCacheReadPerM == nil && v.SetCacheCreationPerM == nil && v.SetPricePerCall == nil && v.SetImgInTokPerM == nil && v.SetImgOutTokPerM == nil && v.SetPricePerImage == nil {
		return fmt.Errorf("%w: variant seq %d requires at least one effect", ErrInvalidInput, v.Seq)
	}
	return nil
}

func validVariantClock(value string) bool {
	if len(value) != 5 || value[2] != ':' || value[0] < '0' || value[0] > '9' || value[1] < '0' || value[1] > '9' || value[3] < '0' || value[3] > '9' || value[4] < '0' || value[4] > '9' {
		return false
	}
	hour := int(value[0]-'0')*10 + int(value[1]-'0')
	minute := int(value[3]-'0')*10 + int(value[4]-'0')
	return hour < 24 && minute < 60
}

func (s *Service) ServiceTierPolicy(tier billing.Tier) billing.TierPolicyMode {
	key := "service_tier_policy_priority"
	if tier == billing.TierFlex {
		key = "service_tier_policy_flex"
	}
	if tier == billing.TierFast {
		key = "service_tier_policy_fast"
	}
	switch s.settingValue(key) {
	case "strip":
		return billing.TierPolicyStrip
	case "reject":
		return billing.TierPolicyReject
	default:
		return billing.TierPolicyPassthrough
	}
}

// ReloadImagePricing intentional dispatch point over unified snapshot (not compat shim).
func (s *Service) ReloadImagePricing()                             { s.ReloadPricing() }
func (s *Service) ReloadImagePricingCtx(ctx context.Context) error { return s.ReloadPricingCtx(ctx) }

// ReloadFunctionPricing intentional dispatch point over unified snapshot (not compat shim).
func (s *Service) ReloadFunctionPricing()                             { s.ReloadPricing() }
func (s *Service) ReloadFunctionPricingCtx(ctx context.Context) error { return s.ReloadPricingCtx(ctx) }

func (s *Service) SetPriceFetcher(f pricing.Fetcher) { s.priceFetcher = f }

func (s *Service) SyncPricingNow(ctx context.Context) (*PricingSyncStats, error) {
	if s.priceFetcher == nil {
		return nil, errors.New("pricing: fetcher not injected")
	}
	// Serialize the fetch as well as the subsequent writes. Locking only the
	// persistence phase lets two requests fetch A/B concurrently and commit A
	// after B when A is slower, silently restoring stale prices. The same mutex
	// is used by the scheduled worker through WithPricingMutation.
	s.pricingMutationMu.Lock()
	defer s.pricingMutationMu.Unlock()
	url := s.settingValue("price_source_url")
	if url == "" {
		return nil, fmt.Errorf("%w: price_source_url not set, skip sync", ErrInvalidInput)
	}
	res, err := s.priceFetcher.Fetch(ctx, url)
	if err != nil {
		if s.log != nil {
			s.log.Warn("pricing sync failed", logx.Error(err))
		}
		return nil, fmt.Errorf("%w: %w", ErrPriceFetch, err)
	}
	if res == nil {
		err := errors.New("pricing: fetch returned nil result")
		if s.log != nil {
			s.log.Warn("pricing sync failed", logx.Error(err))
		}
		return nil, fmt.Errorf("%w: %w", ErrPriceFetch, err)
	}
	entries := res.PriceEntries
	n, err := s.store.UpsertPriceEntriesFromLiteLLM(ctx, entries)
	if err != nil {
		s.reloadPricingUnlocked(ctx)
		return nil, err
	}
	if len(res.Variants) > 0 {
		// Keep the fetched slice immutable while filtering manual rows; the
		// original model list is also used for snapshot reconciliation below.
		filtered := append([]*domain.PriceVariant(nil), res.Variants...)
		manualModels, merr := s.store.ManualEntryModels(ctx)
		if merr != nil {
			// A manual-row lookup is the guard that prevents the separate variant
			// upsert from overwriting administrator-owned pricing.  If the lookup
			// is unavailable, skip variants for this run and surface the error;
			// writing every official variant would silently undo a manual price.
			filtered = nil
			if err == nil {
				err = fmt.Errorf("pricing: list manual price entries: %w", merr)
			}
		} else if len(manualModels) > 0 {
			manualSet := make(map[string]struct{}, len(manualModels))
			for _, m := range manualModels {
				manualSet[m] = struct{}{}
			}
			tmp := filtered[:0]
			for _, v := range filtered {
				if _, isManual := manualSet[v.Model]; !isManual {
					tmp = append(tmp, v)
				}
			}
			filtered = tmp
		}
		if len(filtered) > 0 {
			if verr := func() error {
				_, e := s.store.UpsertPriceVariantsFromLiteLLM(ctx, filtered)
				return e
			}(); verr != nil {
				err = verr
			}
		}
	}
	// Reconcile whenever the source document contains at least one model key.
	// FetchResult.Models is captured before parsing, so Skipped may include legal
	// no-price entries and is intentionally informational rather than a
	// completeness gate. Empty fetches are retained as-is so a transient blank
	// response cannot remove the last known official prices.
	if err == nil {
		if reconciler, ok := s.store.(pricing.SnapshotReconciler); ok {
			models := sourceSnapshotModels(res)
			if lenNonBlankModels(models) > 0 {
				variantModels := make([]string, 0, len(res.Variants))
				for _, variant := range res.Variants {
					if variant != nil {
						variantModels = append(variantModels, variant.Model)
					}
				}
				if _, reconcileErr := reconciler.ReconcileLiteLLMSnapshot(ctx, models, variantModels); reconcileErr != nil {
					err = reconcileErr
				}
			}
		}
	}
	s.reloadPricingUnlocked(ctx)
	if err != nil {
		return nil, err
	}
	return &PricingSyncStats{Rows: len(entries), Skipped: res.Skipped, Updated: n, Variants: len(res.Variants)}, nil
}

// sourceSnapshotModels returns the source key set captured by Parse. The
// fallback preserves compatibility with Fetcher implementations written before
// FetchResult.Models was added; Parse always supplies the authoritative list.
func sourceSnapshotModels(res *pricing.FetchResult) []string {
	if res == nil {
		return nil
	}
	if res.Models != nil {
		return res.Models
	}
	// Fetchers compiled before Models was added cannot provide a source key set.
	// Preserve their historical partial-fetch guard; Parse always initializes
	// Models, including an explicit empty slice.
	if res.Skipped != 0 {
		return nil
	}
	models := make([]string, 0, len(res.PriceEntries))
	for _, entry := range res.PriceEntries {
		if entry != nil {
			models = append(models, entry.Model)
		}
	}
	return models
}

func lenNonBlankModels(models []string) int {
	count := 0
	for _, model := range models {
		if strings.TrimSpace(model) != "" {
			count++
		}
	}
	return count
}

func (s *Service) PreviewPricingSync(ctx context.Context) (*PricingPreview, error) {
	if s.priceFetcher == nil {
		return nil, errors.New("pricing: fetcher not injected")
	}
	url := s.settingValue("price_source_url")
	if url == "" {
		return nil, fmt.Errorf("%w: price_source_url not set, skip sync", ErrInvalidInput)
	}
	res, err := s.priceFetcher.Fetch(ctx, url)
	if err != nil {
		if s.log != nil {
			s.log.Warn("pricing sync preview failed", logx.Error(err))
		}
		return nil, fmt.Errorf("%w: %w", ErrPriceFetch, err)
	}
	if res == nil {
		err := errors.New("pricing: fetch returned nil result")
		if s.log != nil {
			s.log.Warn("pricing sync preview failed", logx.Error(err))
		}
		return nil, fmt.Errorf("%w: %w", ErrPriceFetch, err)
	}
	entries := res.PriceEntries
	snap := s.priceSnapshot.Load()
	preview := &PricingPreview{Skipped: res.Skipped}
	if snap == nil {
		preview.ToAdd = len(entries)
		for _, e := range entries {
			preview.Entries = append(preview.Entries, PricingPreviewEntry{Model: e.Model, Mode: string(e.Mode), Action: "add"})
		}
		preview.VariantsChanged = len(res.Variants)
		return preview, nil
	}
	for _, e := range entries {
		if e == nil {
			continue
		}
		model := strings.TrimSpace(e.Model)
		if _, ok := (*snap).entries[model]; ok {
			preview.ToUpdate++
			preview.Entries = append(preview.Entries, PricingPreviewEntry{Model: e.Model, Mode: string(e.Mode), Action: "update"})
		} else {
			preview.ToAdd++
			preview.Entries = append(preview.Entries, PricingPreviewEntry{Model: e.Model, Mode: string(e.Mode), Action: "add"})
		}
	}
	preview.VariantsChanged = len(res.Variants)
	return preview, nil
}

func (s *Service) PriceSourceURL() string { return s.settingValue("price_source_url") }
func (s *Service) PriceSyncCron() string  { return s.settingValue("price_sync_cron") }
