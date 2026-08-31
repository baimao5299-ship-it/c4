// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/sjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
)

type multiplierRemainderKey struct {
	// Account and key identity are part of the ledger bucket. A user can have
	// several upstream accounts and gateway keys; their fractional balances
	// must never be allowed to settle into one another.
	accountID  int64
	keyID      int64
	userID     int64
	groupID    int64
	model      string
	multiplier int
	prices     resolvedPriceSnapshot
}

// resolvedPriceSnapshot flattens every ResolvedPrices field into a comparable
// value. Presence flags keep an unset price distinct from an explicitly zero
// price, which matters when a live price snapshot is replaced.
type resolvedPriceSnapshot struct {
	mode domain.PriceMode

	inputPerM      int64
	hasInputPerM   bool
	outputPerM     int64
	hasOutputPerM  bool
	cacheReadPerM  int64
	hasCacheRead   bool
	cacheWritePerM int64
	hasCacheWrite  bool
	pricePerCall   int64
	hasPriceCall   bool
	imgInTokPerM   int64
	hasImgInTok    bool
	imgOutTokPerM  int64
	hasImgOutTok   bool
	pricePerImage  int64
	hasPriceImage  bool

	variantSeq    int
	hasVariantSeq bool
	provider      string
	hasProvider   bool
}

type multiplierRemainderCell struct {
	mu                  sync.Mutex
	rawRemainder        int64
	multiplierRemainder int64
	lastUsedUnix        int64
	retired             bool
}

// multiplierRemainderAccumulator preserves both token-price and multiplier
// fractions across requests. A cell belongs to one exact account, gateway key,
// user, group, actual model, multiplier, and resolved-price snapshot, so a
// request from another owner or pricing input cannot consume an older fraction.
// Settled idle cells are retired opportunistically; cells with an unpaid
// fraction stay live.
// A restart can lose less than one raw ledger unit and one multiplier numerator
// remainder per active cell; persistence would require a schema migration.
type multiplierRemainderAccumulator struct {
	values sync.Map // multiplierRemainderKey -> *multiplierRemainderCell
	ops    atomic.Uint64
	cells  atomic.Int64
}

const (
	maxBillingInt64       = int64(1<<63 - 1)
	maxBillingMultiplier  = 100000 // 10x, the validated upper bound (万分数)
	billingMultiplierBase = int64(10000)
	remainderSweepEvery   = uint64(4096)
	remainderIdleSeconds  = int64((24 * time.Hour) / time.Second)
	// maxRemainderCells bounds the number of distinct pricing identities kept
	// in process memory. Unsettled cells are never evicted by age; when this
	// bound is reached, settled cells are trimmed first and a new identity uses
	// conservative immediate rounding if no slot remains.
	maxRemainderCells = int64(65_536)
)

func normalizeBillingMultiplier(m int) int {
	if m < 0 {
		return 0
	}
	if m > maxBillingMultiplier {
		return maxBillingMultiplier
	}
	return m
}

func snapshotResolvedPrices(rp domain.ResolvedPrices) resolvedPriceSnapshot {
	snapshot := resolvedPriceSnapshot{mode: rp.Mode}
	setInt64 := func(dst *int64, present *bool, value *int64) {
		if value == nil {
			return
		}
		*dst = *value
		*present = true
	}
	setInt64(&snapshot.inputPerM, &snapshot.hasInputPerM, rp.InputPerM)
	setInt64(&snapshot.outputPerM, &snapshot.hasOutputPerM, rp.OutputPerM)
	setInt64(&snapshot.cacheReadPerM, &snapshot.hasCacheRead, rp.CacheReadPerM)
	setInt64(&snapshot.cacheWritePerM, &snapshot.hasCacheWrite, rp.CacheWritePerM)
	setInt64(&snapshot.pricePerCall, &snapshot.hasPriceCall, rp.PricePerCall)
	setInt64(&snapshot.imgInTokPerM, &snapshot.hasImgInTok, rp.ImgInTokPerM)
	setInt64(&snapshot.imgOutTokPerM, &snapshot.hasImgOutTok, rp.ImgOutTokPerM)
	setInt64(&snapshot.pricePerImage, &snapshot.hasPriceImage, rp.PricePerImage)
	if rp.VariantSeq != nil {
		snapshot.variantSeq = *rp.VariantSeq
		snapshot.hasVariantSeq = true
	}
	if rp.Provider != nil {
		snapshot.provider = *rp.Provider
		snapshot.hasProvider = true
	}
	return snapshot
}

// PriceResolver 统一价格解析（零 DB 快照读，首中即停变体解析）。
type PriceResolver interface {
	ResolvePrices(model string, promptTokens int64, tier string, at time.Time) (domain.ResolvedPrices, bool)
}

// BillingHooks 计费钩子（proxy.New 参数；nil = 计费全关：不查价、不记
// BillingTier、不处理 service_tier 转发策略、不做余额预检）。
type BillingHooks struct {
	Resolver   PriceResolver
	Balances   *billing.Balances // 余额只读快照（预检 + 扣费后定向刷新）
	Flusher    *billing.Flusher  // 批量扣费落库（billed 路由终点）
	TierPolicy func(tier billing.Tier) billing.TierPolicyMode
}

var (
	// errNoPrice 402：模型缺价（计费启用后未设价/未同步；空价格表 = 全模型 402）。
	errNoPrice = &formatError{status: http.StatusPaymentRequired, msg: "no price configured for this model"}
	// errInsufficientBalance 402：余额预检拒绝（快照缺失或 <0；免费放行路径
	// T3.5 价格倍率扩展）。
	errInsufficientBalance = &formatError{status: http.StatusPaymentRequired, msg: "insufficient balance"}
	// errBillingUnavailable 503：计费已打开但关键余额快照未装配。Failing
	// closed avoids forwarding billable traffic with an unknown balance.
	errBillingUnavailable = &formatError{status: http.StatusServiceUnavailable, msg: "billing temporarily unavailable"}
	// errServiceTierRejected 400：service_tier 策略 reject（不转发，记 ErrBilling）。
	errServiceTierRejected = &formatError{status: http.StatusBadRequest, msg: "service_tier rejected by gateway policy"}
)

// stripServiceTier 删除请求体 service_tier 字段（strip 策略）：sjson 字节级
// 删除（与 WS 面 relayWS 首帧预处理同库同风格，非 map 往返——精度/键序/转义
// 保真同 setModel）。字段缺失时 sjson 删除不存在键 = 无操作返回原字节（与
// map 版 delete 缺失键语义一致）。顶层形状守卫同 setModel（sjson DeleteBytes
// 对标量/数组根无操作返回原字节，map 版报错——调用前 extractTier 已保证
// 对象体，仅防御性一致）。
func stripServiceTier(body []byte) ([]byte, error) {
	if !isJSONObjectRoot(body) {
		return nil, errBodyNotObject
	}
	return sjson.DeleteBytes(body, "service_tier")
}

// applyMultiplier 价格倍率应用（T3.5 用户拍板：整单 round-half-up）：
// (cost×m + 5000)/10000。m==10000（×1 未设置/组默认）恒等短路——默认路径
// 逐指令等价零 round 偏差（函数可内联）。m==0 → cost 0（免费，不扣费）。
// 溢出安全：倍率先钳至校验上限，商/余数分解避免 cost×m 回绕，结果超出
// int64 时饱和到最大值。
func applyMultiplier(cost int64, m int) int64 {
	out, remainder := multiplierFloorParts(cost, m)
	if out == maxBillingInt64 || remainder < billingMultiplierBase/2 {
		return out
	}
	return out + 1
}

// multiplierFloorParts returns floor(cost*m/base) and the exact numerator
// remainder. Splitting the result this way lets the request-level helper keep
// its historical half-up contract while the live billing path aggregates the
// remainder across requests instead of rounding every small request to zero.
func multiplierFloorParts(cost int64, m int) (int64, int64) {
	if cost <= 0 || m <= 0 {
		return 0, 0
	}
	if m == int(billingMultiplierBase) {
		return cost, 0
	}
	if m > maxBillingMultiplier {
		m = maxBillingMultiplier
	}
	// Compute cost*m/base without forming the potentially overflowing product.
	// The remainder product is bounded by 9999*100000, while the quotient part
	// is checked before addition. Saturating protects the int64 ledger contract.
	q, rem := cost/billingMultiplierBase, cost%billingMultiplierBase
	m64 := int64(m)
	if q > maxBillingInt64/m64 {
		return maxBillingInt64, 0
	}
	out := q * m64
	scaledRemainder := rem * m64
	remCost := scaledRemainder / billingMultiplierBase
	if out > maxBillingInt64-remCost {
		return maxBillingInt64, 0
	}
	return out + remCost, scaledRemainder % billingMultiplierBase
}

// apply keeps the old raw-cost helper available for non-model callers. The
// model-aware billing path must use applyResolved so its price snapshot is part
// of the remainder key.
func (a *multiplierRemainderAccumulator) apply(parts billing.CostParts, m int, userID, groupID int64) (int64, int64) {
	return a.applyResolved(parts, m, userID, groupID, "", domain.ResolvedPrices{})
}

// applyResolved emits aggregate raw and billed integer costs. The two-stage
// carry is deliberate: token prices divide by 1M first, then group pricing
// divides by 10,000. Retaining both remainders prevents either stage from
// making a small positive price permanently free. A complete resolved-price
// snapshot and actual model name identify the ledger bucket, so changes to
// either cannot consume a carry produced by an earlier configuration.
func (a *multiplierRemainderAccumulator) applyResolved(parts billing.CostParts, m int, userID, groupID int64, model string, rp domain.ResolvedPrices) (int64, int64) {
	return a.applyResolvedIdentity(parts, m, 0, 0, userID, groupID, model, rp)
}

// applyResolvedIdentity is the production entry point. AccountID and KeyID
// deliberately precede the user/group dimensions so callers cannot silently
// omit the ownership identity when adding a new billing path.
func (a *multiplierRemainderAccumulator) applyResolvedIdentity(parts billing.CostParts, m int, accountID, keyID, userID, groupID int64, model string, rp domain.ResolvedPrices) (int64, int64) {
	m = normalizeBillingMultiplier(m)
	key := multiplierRemainderKey{
		accountID: accountID, keyID: keyID, userID: userID, groupID: groupID, model: model, multiplier: m,
		prices: snapshotResolvedPrices(rp),
	}
	return a.applyKey(parts, key)
}

func (a *multiplierRemainderAccumulator) applyKey(parts billing.CostParts, key multiplierRemainderKey) (int64, int64) {
	now := time.Now().Unix()
	var cell *multiplierRemainderCell
	for {
		stored, loaded := a.values.Load(key)
		if !loaded {
			if !a.reserveCell() {
				// A dirty cell cannot be discarded without losing money. Charge
				// this new identity conservatively in the current request instead
				// of creating an unbounded map entry or silently dropping its
				// fraction.
				a.trimSettled()
				if !a.reserveCell() {
					return applyConservativeCost(parts, key.multiplier)
				}
			}
			candidate := &multiplierRemainderCell{}
			stored, loaded = a.values.LoadOrStore(key, candidate)
			if loaded {
				// Another goroutine won the insertion race; release our slot.
				a.cells.Add(-1)
			}
		}
		cell = stored.(*multiplierRemainderCell)
		cell.mu.Lock()
		if cell.retired {
			cell.mu.Unlock()
			continue
		}
		break
	}
	cell.lastUsedUnix = now

	raw := parts.Units
	if raw < 0 {
		raw = 0
	}
	if raw < maxBillingInt64 {
		remainder := parts.Remainder
		if remainder < 0 {
			remainder = 0
		}
		carry := remainder / billing.CostRemainderScale
		remainder %= billing.CostRemainderScale
		total := cell.rawRemainder + remainder
		carry += total / billing.CostRemainderScale
		cell.rawRemainder = total % billing.CostRemainderScale
		if raw > maxBillingInt64-carry {
			raw = maxBillingInt64
			cell.rawRemainder = 0
		} else {
			raw += carry
		}
	} else {
		cell.rawRemainder = 0
	}

	billed, fraction := multiplierFloorParts(raw, key.multiplier)
	if billed == maxBillingInt64 {
		cell.multiplierRemainder = 0
	} else {
		total := cell.multiplierRemainder + fraction
		carry := total / billingMultiplierBase
		cell.multiplierRemainder = total % billingMultiplierBase
		if billed > maxBillingInt64-carry {
			billed = maxBillingInt64
			cell.multiplierRemainder = 0
		} else {
			billed += carry
		}
	}
	cell.mu.Unlock()

	if a.ops.Add(1)%remainderSweepEvery == 0 {
		a.sweep(now - remainderIdleSeconds)
	}
	return raw, billed
}

func (a *multiplierRemainderAccumulator) sweep(cutoffUnix int64) {
	a.values.Range(func(key, value any) bool {
		cell := value.(*multiplierRemainderCell)
		cell.mu.Lock()
		if !cell.retired && cell.lastUsedUnix < cutoffUnix && cell.rawRemainder == 0 && cell.multiplierRemainder == 0 {
			cell.retired = true
			if a.values.CompareAndDelete(key, cell) {
				a.cells.Add(-1)
			}
		}
		cell.mu.Unlock()
		return true
	})
}

// reserveCell claims one bounded map slot. The count is independent of
// sync.Map's implementation and remains correct under concurrent inserts and
// deletes.
func (a *multiplierRemainderAccumulator) reserveCell() bool {
	for {
		current := a.cells.Load()
		if current >= maxRemainderCells {
			return false
		}
		if a.cells.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// trimSettled aggressively removes only settled cells when the bound is
// reached. Dirty cells remain until a later request for the same identity can
// carry them to a whole ledger unit. This preserves the no-free-usage rule.
func (a *multiplierRemainderAccumulator) trimSettled() {
	a.values.Range(func(key, value any) bool {
		if a.cells.Load() < maxRemainderCells {
			return false
		}
		cell := value.(*multiplierRemainderCell)
		cell.mu.Lock()
		if !cell.retired && cell.rawRemainder == 0 && cell.multiplierRemainder == 0 {
			cell.retired = true
			if a.values.CompareAndDelete(key, cell) {
				a.cells.Add(-1)
			}
		}
		cell.mu.Unlock()
		return true
	})
}

func normalizedCostParts(parts billing.CostParts) (int64, int64) {
	units := parts.Units
	if units < 0 {
		units = 0
	}
	rem := parts.Remainder
	if rem < 0 {
		rem = 0
	}
	return units, rem
}

// applyConservativeCost is the bounded-capacity fallback. Since no cell can
// retain a fraction for a future request, positive fractions are rounded up
// now. This may overstate by less than one ledger unit, but never turns a
// positive cost into a free request or loses an unpaid fraction.
func applyConservativeCost(parts billing.CostParts, multiplier int) (int64, int64) {
	units, rem := normalizedCostParts(parts)
	if units < maxBillingInt64 {
		carry := rem / billing.CostRemainderScale
		if units > maxBillingInt64-carry {
			units = maxBillingInt64
		} else {
			units += carry
		}
	}
	// Any non-zero fractional raw unit is charged immediately when no
	// bounded cell is available. This keeps the fallback conservative while
	// preserving all whole units represented by a large Remainder value.
	if units < maxBillingInt64 && rem%billing.CostRemainderScale != 0 {
		units++
	}
	m := normalizeBillingMultiplier(multiplier)
	if m == 0 || units <= 0 {
		return units, 0
	}
	billed, fraction := multiplierFloorParts(units, m)
	if billed < maxBillingInt64 && fraction > 0 {
		billed++
	}
	return units, billed
}
