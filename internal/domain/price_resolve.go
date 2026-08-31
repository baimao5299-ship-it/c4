// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"fmt"
	"time"
)

// 价格解析核：entry → ResolvedPrices 基底 + 首中即停变体应用。纯函数、
// 零外部依赖——service 快照读与测试假实现共用同一实现（防逻辑漂移）。

// ResolveEntryPrices 统一价格解析：基底取 entry 分量，按 seq 升序找第一条
// 匹配（tier/上下文/时段/星期 AND 组合）的变体应用 mult_bp/set_* 后返回。
// variants 无需预排序——调用方按 seq 升序传入；首中即停。
func ResolveEntryPrices(entry *PriceEntry, variants []*PriceVariant, tier string, promptTokens int64, at time.Time) (ResolvedPrices, bool) {
	if entry == nil {
		return ResolvedPrices{}, false
	}
	rp := ResolvedPrices{
		Mode:           entry.Mode,
		InputPerM:      entry.InputPerM,
		OutputPerM:     entry.OutputPerM,
		CacheReadPerM:  entry.CacheReadPerM,
		CacheWritePerM: entry.CacheWritePerM,
		PricePerCall:   entry.PricePerCall,
		ImgInTokPerM:   entry.ImgInTokPerM,
		ImgOutTokPerM:  entry.ImgOutTokPerM,
		PricePerImage:  entry.PricePerImage,
		Provider:       entry.Provider,
	}
	for _, v := range variants {
		if !variantMatches(v, tier, promptTokens, at) {
			continue
		}
		seq := v.Seq
		rp.VariantSeq = &seq
		if v.MultBP != nil {
			mult := int64(*v.MultBP)
			if mult < 0 || mult > 100000 {
				return ResolvedPrices{}, false
			}
			applyMult := func(p **int64, overridden bool) bool {
				if overridden || *p == nil {
					return true
				}
				val, ok := priceMultiplierExact(**p, mult)
				if !ok {
					return false
				}
				// 复制新指针，禁止改写快照内分量（并发读者可见性）
				nv := val
				*p = &nv
				return true
			}
			if !applyMult(&rp.InputPerM, v.SetInputPerM != nil) ||
				!applyMult(&rp.OutputPerM, v.SetOutputPerM != nil) ||
				!applyMult(&rp.CacheReadPerM, v.SetCacheReadPerM != nil) ||
				!applyMult(&rp.CacheWritePerM, v.SetCacheCreationPerM != nil) ||
				!applyMult(&rp.PricePerCall, v.SetPricePerCall != nil) ||
				!applyMult(&rp.ImgInTokPerM, v.SetImgInTokPerM != nil) ||
				!applyMult(&rp.ImgOutTokPerM, v.SetImgOutTokPerM != nil) ||
				!applyMult(&rp.PricePerImage, v.SetPricePerImage != nil) {
				return ResolvedPrices{}, false
			}
		}
		if v.SetInputPerM != nil {
			nv := *v.SetInputPerM
			rp.InputPerM = &nv
		}
		if v.SetOutputPerM != nil {
			nv := *v.SetOutputPerM
			rp.OutputPerM = &nv
		}
		if v.SetCacheReadPerM != nil {
			nv := *v.SetCacheReadPerM
			rp.CacheReadPerM = &nv
		}
		if v.SetCacheCreationPerM != nil {
			nv := *v.SetCacheCreationPerM
			rp.CacheWritePerM = &nv
		}
		if v.SetPricePerCall != nil {
			nv := *v.SetPricePerCall
			rp.PricePerCall = &nv
		}
		if v.SetImgInTokPerM != nil {
			nv := *v.SetImgInTokPerM
			rp.ImgInTokPerM = &nv
		}
		if v.SetImgOutTokPerM != nil {
			nv := *v.SetImgOutTokPerM
			rp.ImgOutTokPerM = &nv
		}
		if v.SetPricePerImage != nil {
			nv := *v.SetPricePerImage
			rp.PricePerImage = &nv
		}
		break
	}
	return rp, true
}

const maxPriceInt64 = int64(1<<63 - 1)

// priceMultiplierExact applies a bounded multiplier only when the result stays
// exactly on the persisted 1e-5 USD grid. Silently rounding here can turn a
// positive conditional price into free traffic or charge twice its configured
// amount, so legacy values that cannot be represented fail closed.
func priceMultiplierExact(price, multiplier int64) (int64, bool) {
	if price < 0 || multiplier < 0 || multiplier > 100000 {
		return 0, false
	}
	if multiplier == 0 || price == 0 {
		return 0, true
	}
	const base int64 = 10000
	q, rem := price/base, price%base
	if q > maxPriceInt64/multiplier {
		return 0, false
	}
	out := q * multiplier
	remProduct := rem * multiplier
	if remProduct%base != 0 {
		return 0, false
	}
	remValue := remProduct / base
	if out > maxPriceInt64-remValue {
		return 0, false
	}
	return out + remValue, true
}

// ValidateVariantPricePrecision checks a stored variant against the current
// base price before an admin write. Explicit per-field overrides run after the
// multiplier and therefore do not depend on the multiplied base component.
func ValidateVariantPricePrecision(entry *PriceEntry, v *PriceVariant) error {
	if entry == nil || v == nil || v.MultBP == nil {
		return nil
	}
	mult := int64(*v.MultBP)
	for _, field := range []struct {
		name       string
		base       *int64
		overridden bool
	}{
		{"input_per_m", entry.InputPerM, v.SetInputPerM != nil},
		{"output_per_m", entry.OutputPerM, v.SetOutputPerM != nil},
		{"cache_read_per_m", entry.CacheReadPerM, v.SetCacheReadPerM != nil},
		{"cache_write_per_m", entry.CacheWritePerM, v.SetCacheCreationPerM != nil},
		{"price_per_call", entry.PricePerCall, v.SetPricePerCall != nil},
		{"img_in_tok_per_m", entry.ImgInTokPerM, v.SetImgInTokPerM != nil},
		{"img_out_tok_per_m", entry.ImgOutTokPerM, v.SetImgOutTokPerM != nil},
		{"price_per_image", entry.PricePerImage, v.SetPricePerImage != nil},
	} {
		if field.overridden || field.base == nil {
			continue
		}
		if _, ok := priceMultiplierExact(*field.base, mult); !ok {
			return fmt.Errorf("variant multiplier makes %s unrepresentable at 0.00001 USD precision", field.name)
		}
	}
	return nil
}

// variantMatches 变体条件匹配：全 nil = 通配；非 nil 条件全过才命中。
func variantMatches(v *PriceVariant, tier string, promptTokens int64, at time.Time) bool {
	if v == nil {
		return false
	}
	if v.ServiceTier != nil && *v.ServiceTier != tier {
		return false
	}
	if v.CtxMin != nil && promptTokens < *v.CtxMin {
		return false
	}
	if v.CtxMax != nil && promptTokens >= *v.CtxMax {
		return false
	}
	if v.TimeStart != nil || v.TimeEnd != nil {
		if !timeMatches(v.TimeStart, v.TimeEnd, at) {
			return false
		}
	}
	if v.DowMask != nil {
		wd := int(at.Weekday()) // 0=Sun
		if (*v.DowMask>>wd)&1 == 0 {
			return false
		}
	}
	return true
}

func timeMatches(start, end *string, at time.Time) bool {
	if start == nil && end == nil {
		return true
	}
	// parse HH:MM
	parse := func(s string) int {
		var h, m int
		fmt.Sscanf(s, "%d:%d", &h, &m)
		return h*60 + m
	}
	cur := at.Hour()*60 + at.Minute()
	if start != nil && end != nil {
		s := parse(*start)
		e := parse(*end)
		if s == e {
			return true
		}
		if s < e {
			return cur >= s && cur < e
		}
		// midnight wrap
		return cur >= s || cur < e
	}
	if start != nil {
		s := parse(*start)
		return cur >= s
	}
	if end != nil {
		e := parse(*end)
		return cur < e
	}
	return true
}
