// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package proxy

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/stretchr/testify/require"
)

func remainderProxy(t *testing.T, multiplier int) *Proxy {
	t.Helper()
	balances := billing.NewBalances(fakeBalanceLoader{gm: map[int64]int{7: multiplier}}, nil)
	require.NoError(t, balances.Reload(t.Context()))
	return &Proxy{bill: &BillingHooks{Balances: balances}}
}

func remainderCellCount(a *multiplierRemainderAccumulator) int {
	count := 0
	a.values.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

func TestApplyMultiplierLogAggregatesSubUnitCost(t *testing.T) {
	p := remainderProxy(t, 1) // x0.0001; one raw unit yields 1/10000 ledger unit
	var emitted int64
	for i := 0; i < 10_000; i++ {
		log := &domain.UsageLog{UserID: 11, GroupID: 7}
		p.applyMultiplierLog(log, 1)
		emitted += log.Cost
	}
	require.Equal(t, int64(1), emitted, "fractional multiplier must carry into one billable unit")

	// The accumulator is keyed by user and group, so another key starts with a
	// clean remainder and cannot receive the first key's carry.
	for i := 0; i < 9_999; i++ {
		log := &domain.UsageLog{UserID: 12, GroupID: 7}
		p.applyMultiplierLog(log, 1)
		require.Zero(t, log.Cost)
	}
	last := &domain.UsageLog{UserID: 12, GroupID: 7}
	p.applyMultiplierLog(last, 1)
	require.Equal(t, int64(1), last.Cost)
}

func TestProxyBillingPointZeroZeroOneMultiplierAggregatesEndToEnd(t *testing.T) {
	upstream := fakeOpenAI(t, "")
	defer upstream.Close()
	store := &captureLogStore{}
	recorder := usage.New(usage.UsageConfig{
		BatchSize: 200, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour,
	}, store, nil)
	balances := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 50_000}, gm: map[int64]int{10: 10}, // x0.001
	}, nil)
	require.NoError(t, balances.Reload(context.Background()))
	p := newTestProxyBillingT3Logs(t, upstream.URL, &fakePriceLookup{
		entries:  map[string]*domain.PriceEntry{"gpt-4o": proxyPricingEntry()},
		variants: map[string][]*domain.PriceVariant{"gpt-4o": proxyPricingVariants()},
	}, balances, recorder)

	// fakeOpenAI reports a raw cost of 130 per request. At x0.001, one hundred
	// requests have an exact aggregate cost of 13 ledger units. Per-request
	// rounding used to emit zero for every row and lose the entire amount.
	for i := 0; i < 100; i++ {
		chatCostReq(t, p)
	}
	require.NoError(t, recorder.Close(context.Background()))

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 100)
	var rawTotal, billedTotal int64
	for _, log := range store.logs {
		rawTotal += log.RawCost
		billedTotal += log.Cost
	}
	require.Equal(t, int64(13_000), rawTotal)
	require.Equal(t, int64(13), billedTotal, "x0.001 aggregate must reach the ledger instead of rounding every request to zero")
}

func TestApplyBillingPartsAggregatesTinyTokenPrice(t *testing.T) {
	// $0.001 / 1M tokens is stored as 100 ledger sub-units. Ten requests of
	// 1,000 tokens each cost exactly one ledger unit in aggregate; rounding each
	// request independently would emit zero for all ten.
	p := remainderProxy(t, 10_000)
	price := int64(100)
	rp := domain.ResolvedPrices{InputPerM: &price}
	var rawTotal, billedTotal int64
	for i := 0; i < 10; i++ {
		log := &domain.UsageLog{UserID: 51, GroupID: 7, InputTokens: 1_000}
		p.applyBillingParts(log, billing.CostPartsFromResolved(rp, log.InputTokens, 0, 0, 0))
		rawTotal += log.RawCost
		billedTotal += log.Cost
	}
	require.Equal(t, int64(1), rawTotal, "raw token cost must carry fractions across requests")
	require.Equal(t, int64(1), billedTotal, "a positive $0.001/M price must not remain free")
}

func TestApplyMultiplierLogConcurrentRemaindersAreLossless(t *testing.T) {
	p := remainderProxy(t, 1)
	const workers = 16
	const requestsPerWorker = 10_000
	var total atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < requestsPerWorker; j++ {
				log := &domain.UsageLog{UserID: 21, GroupID: 7}
				p.applyMultiplierLog(log, 1)
				total.Add(log.Cost)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(workers*requestsPerWorker/10_000), total.Load(), "concurrent carries must not be lost")
}

func TestApplyMultiplierLogPreservesFreeAndIdentityMultipliers(t *testing.T) {
	free := remainderProxy(t, 0)
	for i := 0; i < 20_000; i++ {
		log := &domain.UsageLog{UserID: 31, GroupID: 7}
		free.applyMultiplierLog(log, 123)
		require.Zero(t, log.Cost, "m=0 must remain free for every request")
	}

	identity := remainderProxy(t, 10_000)
	for _, raw := range []int64{1, 7, 123, 1_000_001} {
		log := &domain.UsageLog{UserID: 31, GroupID: 7}
		identity.applyMultiplierLog(log, raw)
		require.Equal(t, raw, log.Cost, "m=10000 must be exact")
	}
}

func TestApplyMultiplierLogDoesNotCarryAcrossMultiplierChanges(t *testing.T) {
	// Keep a fractional x0.0001 remainder, then change to x0.0002. The old
	// carry must not be applied to the new rate.
	p := remainderProxy(t, 1)
	first := &domain.UsageLog{UserID: 41, GroupID: 7}
	p.applyMultiplierLog(first, 1)
	require.Zero(t, first.Cost)

	p.bill.Balances = billing.NewBalances(fakeBalanceLoader{gm: map[int64]int{7: 2}}, nil)
	require.NoError(t, p.bill.Balances.Reload(context.Background()))
	for i := 0; i < 4_999; i++ {
		log := &domain.UsageLog{UserID: 41, GroupID: 7}
		p.applyMultiplierLog(log, 1)
		require.Zero(t, log.Cost)
	}
	last := &domain.UsageLog{UserID: 41, GroupID: 7}
	p.applyMultiplierLog(last, 1)
	require.Equal(t, int64(1), last.Cost, "new multiplier needs its own 5000-request carry")
	require.Equal(t, 2, remainderCellCount(&p.billingRemainders), "each multiplier has an isolated remainder cell")
}

func TestTokenPriceRemainderResetsAcrossMultiplierChanges(t *testing.T) {
	p := remainderProxy(t, 10_000)
	parts := billing.CostParts{Remainder: billing.CostRemainderScale / 2}
	first := &domain.UsageLog{UserID: 61, GroupID: 7}
	p.applyBillingParts(first, parts)
	require.Zero(t, first.RawCost)

	p.bill.Balances = billing.NewBalances(fakeBalanceLoader{gm: map[int64]int{7: 20_000}}, nil)
	require.NoError(t, p.bill.Balances.Reload(context.Background()))
	second := &domain.UsageLog{UserID: 61, GroupID: 7}
	p.applyBillingParts(second, parts)
	require.Zero(t, second.RawCost, "the previous pricing period must not carry into a new multiplier")
	third := &domain.UsageLog{UserID: 61, GroupID: 7}
	p.applyBillingParts(third, parts)
	require.Equal(t, int64(1), third.RawCost)
	require.Equal(t, int64(2), third.Cost)
	require.Equal(t, 2, remainderCellCount(&p.billingRemainders))
}

func TestResolvedRemainderDoesNotCrossModels(t *testing.T) {
	var a multiplierRemainderAccumulator
	price := int64(200_000)
	rp := domain.ResolvedPrices{Mode: domain.PriceModeToken, InputPerM: &price}

	raw, billed := a.applyResolved(billing.CostPartsFromResolved(rp, 3, 0, 0, 0), 10_000, 81, 7, "model-a", rp)
	require.Zero(t, raw)
	require.Zero(t, billed)
	raw, billed = a.applyResolved(billing.CostPartsFromResolved(rp, 2, 0, 0, 0), 10_000, 81, 7, "model-b", rp)
	require.Zero(t, raw, "model-b must not consume model-a's remainder")
	require.Zero(t, billed)

	raw, billed = a.applyResolved(billing.CostPartsFromResolved(rp, 2, 0, 0, 0), 10_000, 81, 7, "model-a", rp)
	require.Equal(t, int64(1), raw)
	require.Equal(t, int64(1), billed)
	raw, billed = a.applyResolved(billing.CostPartsFromResolved(rp, 3, 0, 0, 0), 10_000, 81, 7, "model-b", rp)
	require.Equal(t, int64(1), raw)
	require.Equal(t, int64(1), billed)
}

func TestResolvedRemainderDoesNotCrossAccountsOrKeys(t *testing.T) {
	var a multiplierRemainderAccumulator
	price := int64(500_000)
	rp := domain.ResolvedPrices{Mode: domain.PriceModeToken, InputPerM: &price}
	parts := billing.CostPartsFromResolved(rp, 1, 0, 0, 0)

	// One token at $0.50/M leaves a half-unit raw remainder. A different
	// upstream account or gateway key must not receive that carry.
	raw, billed := a.applyResolvedIdentity(parts, 10_000, 101, 201, 1, 7, "model", rp)
	require.Zero(t, raw)
	require.Zero(t, billed)

	raw, billed = a.applyResolvedIdentity(parts, 10_000, 102, 201, 1, 7, "model", rp)
	require.Zero(t, raw, "a different upstream account must have an independent fraction")
	require.Zero(t, billed)

	raw, billed = a.applyResolvedIdentity(parts, 10_000, 101, 202, 1, 7, "model", rp)
	require.Zero(t, raw, "a different gateway key must have an independent fraction")
	require.Zero(t, billed)

	raw, billed = a.applyResolvedIdentity(parts, 10_000, 101, 201, 1, 7, "model", rp)
	require.Equal(t, int64(1), raw)
	require.Equal(t, int64(1), billed, "only the original account/key bucket may consume its carry")
}

func TestResolvedRemainderDoesNotCrossPriceSnapshots(t *testing.T) {
	var a multiplierRemainderAccumulator
	priceA, priceB := int64(600_000), int64(400_000)
	rpA := domain.ResolvedPrices{Mode: domain.PriceModeToken, InputPerM: &priceA}
	rpB := domain.ResolvedPrices{Mode: domain.PriceModeToken, InputPerM: &priceB}

	raw, _ := a.applyResolved(billing.CostPartsFromResolved(rpA, 1, 0, 0, 0), 10_000, 82, 7, "model", rpA)
	require.Zero(t, raw)
	raw, _ = a.applyResolved(billing.CostPartsFromResolved(rpB, 1, 0, 0, 0), 10_000, 82, 7, "model", rpB)
	require.Zero(t, raw, "a new price must not consume the previous price's remainder")

	raw, _ = a.applyResolved(billing.CostPartsFromResolved(rpA, 1, 0, 0, 0), 10_000, 82, 7, "model", rpA)
	require.Equal(t, int64(1), raw)
	_, _ = a.applyResolved(billing.CostPartsFromResolved(rpB, 1, 0, 0, 0), 10_000, 82, 7, "model", rpB)
	raw, _ = a.applyResolved(billing.CostPartsFromResolved(rpB, 1, 0, 0, 0), 10_000, 82, 7, "model", rpB)
	require.Equal(t, int64(1), raw)
}

func TestResolvedPriceSnapshotIncludesEveryField(t *testing.T) {
	provider := "provider-a"
	variant := 3
	values := []int64{11, 12, 13, 14, 15, 16, 17, 18}
	base := domain.ResolvedPrices{
		Mode: domain.PriceModeToken, InputPerM: &values[0], OutputPerM: &values[1],
		CacheReadPerM: &values[2], CacheWritePerM: &values[3], PricePerCall: &values[4],
		ImgInTokPerM: &values[5], ImgOutTokPerM: &values[6], PricePerImage: &values[7],
		VariantSeq: &variant, Provider: &provider,
	}
	wantDifferent := []struct {
		name   string
		mutate func(*domain.ResolvedPrices)
	}{
		{"mode", func(r *domain.ResolvedPrices) { r.Mode = domain.PriceModeImage }},
		{"input", func(r *domain.ResolvedPrices) { r.InputPerM = ptr(int64(101)) }},
		{"output", func(r *domain.ResolvedPrices) { r.OutputPerM = ptr(int64(102)) }},
		{"cache read", func(r *domain.ResolvedPrices) { r.CacheReadPerM = ptr(int64(103)) }},
		{"cache write", func(r *domain.ResolvedPrices) { r.CacheWritePerM = ptr(int64(104)) }},
		{"call", func(r *domain.ResolvedPrices) { r.PricePerCall = ptr(int64(105)) }},
		{"image input", func(r *domain.ResolvedPrices) { r.ImgInTokPerM = ptr(int64(106)) }},
		{"image output", func(r *domain.ResolvedPrices) { r.ImgOutTokPerM = ptr(int64(107)) }},
		{"image", func(r *domain.ResolvedPrices) { r.PricePerImage = ptr(int64(108)) }},
		{"variant", func(r *domain.ResolvedPrices) { r.VariantSeq = ptr(4) }},
		{"provider", func(r *domain.ResolvedPrices) { r.Provider = ptr("provider-b") }},
		{"nil differs from zero", func(r *domain.ResolvedPrices) { r.InputPerM = nil }},
	}
	baseline := snapshotResolvedPrices(base)
	for _, tc := range wantDifferent {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.mutate(&changed)
			require.NotEqual(t, baseline, snapshotResolvedPrices(changed))
		})
	}
}

func TestResolvedRemainderFreeMultiplierNeverCarriesIntoPaidPricing(t *testing.T) {
	var a multiplierRemainderAccumulator
	price := int64(500_000)
	rp := domain.ResolvedPrices{Mode: domain.PriceModeToken, InputPerM: &price}
	parts := billing.CostPartsFromResolved(rp, 1, 0, 0, 0)
	var freeRaw, freeBilled int64
	for range 2 {
		raw, billed := a.applyResolved(parts, 0, 83, 7, "model", rp)
		freeRaw += raw
		freeBilled += billed
	}
	require.Equal(t, int64(1), freeRaw, "free traffic still records its raw cost")
	require.Zero(t, freeBilled, "free traffic must never debit the user")

	raw, billed := a.applyResolved(parts, 10_000, 83, 7, "model", rp)
	require.Zero(t, raw, "the free bucket must not carry into the paid bucket")
	require.Zero(t, billed)
	raw, billed = a.applyResolved(parts, 10_000, 83, 7, "model", rp)
	require.Equal(t, int64(1), raw)
	require.Equal(t, int64(1), billed)
}

func TestResolvedRemainderConcurrentBasePriceCarriesAreLossless(t *testing.T) {
	var a multiplierRemainderAccumulator
	price := int64(250_000)
	rp := domain.ResolvedPrices{Mode: domain.PriceModeToken, InputPerM: &price}
	parts := billing.CostPartsFromResolved(rp, 1, 0, 0, 0)
	const workers = 16
	const perWorker = 1_000
	var rawTotal, billedTotal atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				raw, billed := a.applyResolved(parts, 10_000, 84, 7, "model", rp)
				rawTotal.Add(raw)
				billedTotal.Add(billed)
			}
		}()
	}
	wg.Wait()
	want := int64(workers * perWorker / 4)
	require.Equal(t, want, rawTotal.Load())
	require.Equal(t, want, billedTotal.Load())
}

func TestResolvedRemainderNormalizesLargeRemainderWithoutOverflow(t *testing.T) {
	var a multiplierRemainderAccumulator
	parts := billing.CostParts{Remainder: maxBillingInt64}
	raw, billed := a.applyResolved(parts, 10_000, 85, 7, "model", domain.ResolvedPrices{})
	want := maxBillingInt64 / billing.CostRemainderScale
	require.Equal(t, want, raw)
	require.Equal(t, want, billed)

	remaining := maxBillingInt64 % billing.CostRemainderScale
	raw, billed = a.applyResolved(
		billing.CostParts{Remainder: billing.CostRemainderScale - remaining},
		10_000, 85, 7, "model", domain.ResolvedPrices{},
	)
	require.Equal(t, int64(1), raw)
	require.Equal(t, int64(1), billed)
}

func TestBillingRemainderSweepKeepsUnsettledIdleCells(t *testing.T) {
	p := remainderProxy(t, 10_000)
	log := &domain.UsageLog{UserID: 71, GroupID: 7}
	p.applyBillingParts(log, billing.CostParts{Remainder: 1})
	require.Equal(t, 1, remainderCellCount(&p.billingRemainders))
	p.billingRemainders.sweep(time.Now().Add(time.Second).Unix())
	require.Equal(t, 1, remainderCellCount(&p.billingRemainders), "an unpaid fraction must not expire into free usage")
}

func TestBillingRemainderSweepRetiresSettledIdleCells(t *testing.T) {
	p := remainderProxy(t, 10_000)
	log := &domain.UsageLog{UserID: 72, GroupID: 7}
	p.applyBillingParts(log, billing.CostParts{Units: 1})
	require.Equal(t, 1, remainderCellCount(&p.billingRemainders))
	p.billingRemainders.sweep(time.Now().Add(time.Second).Unix())
	require.Zero(t, remainderCellCount(&p.billingRemainders))
}

func TestBillingRemainderCapacityKeepsDirtyCellsAndChargesNewIdentity(t *testing.T) {
	var a multiplierRemainderAccumulator
	price := int64(500_000)
	rp := domain.ResolvedPrices{Mode: domain.PriceModeToken, InputPerM: &price}
	parts := billing.CostPartsFromResolved(rp, 1, 0, 0, 0)

	// Fill every slot with an unpaid fraction. The capacity fallback must not
	// evict those cells; a new identity is charged conservatively instead.
	for i := int64(0); i < maxRemainderCells; i++ {
		_, _ = a.applyResolvedIdentity(parts, 10_000, 9000+i, 1, 1, 7, "model", rp)
	}
	require.Equal(t, maxRemainderCells, a.cells.Load())
	before := remainderCellCount(&a)
	raw, billed := a.applyResolvedIdentity(parts, 10_000, 999999, 1, 1, 7, "model", rp)
	require.Equal(t, before, remainderCellCount(&a), "dirty cells remain retained at capacity")
	require.Equal(t, int64(1), raw, "capacity fallback rounds positive raw fraction up")
	require.Equal(t, int64(1), billed, "capacity fallback never emits a free positive request")
}
