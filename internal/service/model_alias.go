// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"sort"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
)

// modelLookupKey resolves a provider model identifier to one persisted pricing
// row. Exact identifiers always win. A fallback is accepted only when its
// candidate is unambiguous; choosing one of several provider-specific prices
// would make the model plaza display a number that may belong to another
// upstream.
func modelLookupKey(keys []string, requested string) (string, bool) {
	candidates := modelLookupCandidates(keys, requested)
	if len(candidates) != 1 {
		return "", false
	}
	return candidates[0], true
}

// modelLookupKeyCoalesced keeps the ambiguity guard for provider-qualified
// aliases, but accepts several candidates when the caller can prove that they
// have the same effective value. LiteLLM commonly publishes one row per
// provider; rejecting every unqualified alias made the model plaza show a
// missing price even when all providers charged the same amount. A caller
// must supply the equality check so this helper never guesses across different
// prices or conditional variants.
func modelLookupKeyCoalesced(keys []string, requested string, equivalent func(left, right string) bool) (string, bool) {
	candidates := modelLookupCandidates(keys, requested)
	if len(candidates) == 0 || equivalent == nil {
		return "", false
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	for _, candidate := range candidates[1:] {
		if !equivalent(candidates[0], candidate) {
			return "", false
		}
	}
	// keys are sorted before this helper is called, so selecting the first
	// equal candidate is deterministic and does not depend on map iteration.
	return candidates[0], true
}

func modelLookupCandidates(keys []string, requested string) []string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil
	}
	for _, key := range keys {
		if key == requested {
			return []string{key}
		}
	}

	requestedBase := modelBasename(requested)
	requestedSnapshot, requestedHasSnapshot := stripModelSnapshot(requested)
	requestedSnapshotBase := modelBasename(requestedSnapshot)
	requestedQualified := strings.Contains(requested, "/")
	bestRank := 0
	best := make([]string, 0, 2)
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		rank := modelAliasRank(key, requested, requestedBase, requestedSnapshot, requestedSnapshotBase, requestedHasSnapshot, requestedQualified)
		if rank == 0 {
			continue
		}
		if rank > bestRank {
			bestRank = rank
			best = best[:0]
		}
		if rank == bestRank && !containsString(best, key) {
			best = append(best, key)
		}
	}
	return best
}

func modelAliasRank(key, requested, requestedBase, requestedSnapshot, requestedSnapshotBase string, requestedHasSnapshot, requestedQualified bool) int {
	keyBase := modelBasename(key)
	keySnapshot, keyHasSnapshot := stripModelSnapshot(key)
	keySnapshotBase := modelBasename(keySnapshot)
	keyQualified := strings.Contains(key, "/")
	// A qualified request identifies a provider namespace as well as the model
	// name. Never borrow a price from a different qualified namespace: a single
	// remaining candidate is not proof that two providers charge the same.
	// Falling back to an unqualified canonical row remains supported below.
	if requestedQualified && keyQualified && modelNamespace(key) != modelNamespace(requested) {
		return 0
	}
	// Some providers encode the same numeric release as either `2.0` or
	// `2-0` (Volcengine's doubao catalogue is a common example). Normalize
	// only separators between short numeric components; broad punctuation
	// folding would merge unrelated routes such as `foo.bar` and `foo-bar`.
	keyNumericBase := numericModelAlias(keyBase)
	requestedNumericBase := numericModelAlias(requestedBase)
	keyNumericSnapshotBase := numericModelAlias(keySnapshotBase)
	requestedNumericSnapshotBase := numericModelAlias(requestedSnapshotBase)
	if key == requested {
		return 100
	}
	// For an unqualified upstream model, prefer a root catalogue row over an
	// arbitrary provider-qualified row. This is the safe fallback for IDs such
	// as deepseek-v4-pro-0813 when the catalogue has only deepseek-v4-pro as its
	// canonical row and several provider-specific snapshot rows.
	if !requestedQualified && !keyQualified && requestedHasSnapshot && key == requestedSnapshot {
		return 90
	}
	if keyBase == requestedBase {
		// A provider-qualified catalogue row is safe for an unqualified
		// request only when it is the single best candidate. Root rows still
		// outrank this fallback (rank 80 vs 55), while multiple providers are
		// rejected by modelLookupKey's ambiguity check.
		if !requestedQualified && keyQualified {
			return 55
		}
		return 80
	}
	// Keep billing lookup aligned with scheduler routing for cosmetic provider
	// spellings such as `model.1`, `model-1`, and `MODEL_1`.
	if strings.EqualFold(modelMatchKey(keyBase), modelMatchKey(requestedBase)) {
		if !requestedQualified && keyQualified {
			return 55
		}
		return 79
	}
	// Relay catalogues frequently vary only in case and in the separator used
	// inside a numeric release (for example Claude-Fable-5.1 versus
	// claude-fable-5-1). Exact IDs still win above. The candidate collector and
	// price-equivalence guard keep this fallback deterministic when a catalogue
	// happens to contain several spellings with different prices.
	if strings.EqualFold(keyNumericBase, requestedNumericBase) {
		if !requestedQualified && keyQualified {
			return 55
		}
		return 79
	}
	// An unqualified request often omits the provider's dated suffix while the
	// official row includes it. Prefer an undated root (rank 80), but accept one
	// dated row when no root exists. The caller still rejects ambiguous rows.
	if keyHasSnapshot && !requestedHasSnapshot && keySnapshotBase == requestedBase {
		if !requestedQualified && keyQualified {
			return 54
		}
		return 78
	}
	// Apply the same release fallback after conservative numeric separator
	// normalization, covering `doubao-seed-2.0-pro` ↔ `...-2-0-pro`.
	if keyNumericBase == requestedNumericBase && keyNumericBase != keyBase {
		if !requestedQualified && keyQualified {
			return 54
		}
		return 79
	}
	if keyHasSnapshot && !requestedHasSnapshot && keyNumericSnapshotBase == requestedNumericBase {
		if !requestedQualified && keyQualified {
			return 53
		}
		return 77
	}
	// Provider catalogues are not consistent about basename casing. Permit
	// this normalization only for a namespaced candidate; direct model IDs
	// remain case-sensitive so billing never silently changes an identifier.
	if !requestedQualified && keyQualified && strings.EqualFold(keyBase, requestedBase) {
		return 55
	}
	if requestedHasSnapshot && keyHasSnapshot && keySnapshot == requestedSnapshot {
		if !requestedQualified && !keyQualified {
			return 75
		}
		return 70
	}
	if requestedHasSnapshot && keyHasSnapshot && keySnapshotBase == requestedSnapshotBase {
		if !requestedQualified && !keyQualified {
			return 65
		}
		return 60
	}
	if requestedHasSnapshot && keyHasSnapshot && keyNumericSnapshotBase == requestedNumericSnapshotBase && keySnapshotBase != requestedSnapshotBase {
		if !requestedQualified && !keyQualified {
			return 64
		}
		return 59
	}
	if requestedHasSnapshot && !requestedQualified && keyQualified && keyHasSnapshot && strings.EqualFold(keySnapshotBase, requestedSnapshotBase) {
		// As with an undated provider row, only a namespaced candidate gets
		// case-folding. modelLookupKey still rejects multiple equal-rank rows.
		return 55
	}
	// Short aliases are accepted only for an unqualified token such as `k3`
	// and only when that token is an actual component of the catalogue ID.
	// modelLookupKey rejects multiple candidates; priceLookupKey additionally
	// permits them only when every candidate has an identical billable value.
	if !requestedQualified && shortModelAliasMatches(keyBase, requestedBase) {
		if keyQualified {
			return 35
		}
		return 40
	}
	// A provider-qualified request can still use a uniquely matching basename
	// row when a relay omitted its namespace. Never apply this to an unqualified
	// request after the root preference above, as that would mix providers.
	if requestedQualified && keySnapshotBase == requestedSnapshotBase {
		return 50
	}
	return 0
}

func shortModelAliasMatches(model, requested string) bool {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if !isShortModelToken(requested) {
		return false
	}
	for _, part := range strings.FieldsFunc(strings.ToLower(strings.TrimSpace(model)), func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	}) {
		if part == requested {
			return true
		}
	}
	return false
}

func isShortModelToken(value string) bool {
	if len(value) < 2 || len(value) > 4 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// numericModelAlias folds only '-'/'_' separators between two short numeric
// components to '.'. Provider IDs use hyphens for both version numbers
// (`claude-opus-4-6`) and ordinary dimensions (`qwen3-235b`); limiting each
// component to at most two digits avoids rewriting the latter while covering
// common 2.0/2-0 and 4.6/4-6 spellings.
func numericModelAlias(model string) string {
	if model == "" {
		return model
	}
	var out strings.Builder
	out.Grow(len(model))
	for i := 0; i < len(model); {
		if model[i] < '0' || model[i] > '9' {
			out.WriteByte(model[i])
			i++
			continue
		}
		start := i
		for i < len(model) && model[i] >= '0' && model[i] <= '9' {
			i++
		}
		leftLen := i - start
		if i < len(model) && (model[i] == '-' || model[i] == '_') {
			sep := i
			right := i + 1
			for right < len(model) && model[right] >= '0' && model[right] <= '9' {
				right++
			}
			rightLen := right - (sep + 1)
			if leftLen <= 2 && rightLen > 0 && rightLen <= 2 {
				out.WriteString(model[start:i])
				out.WriteByte('.')
				out.WriteString(model[sep+1 : right])
				i = right
				continue
			}
		}
		out.WriteString(model[start:i])
	}
	return out.String()
}

func modelBasename(model string) string {
	model = strings.TrimSpace(model)
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 && slash+1 < len(model) {
		return model[slash+1:]
	}
	return model
}

func modelMatchKey(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(model))
	lastDash := false
	for _, r := range model {
		if r == '_' || r == '.' {
			r = '-'
		}
		if r == '-' {
			if lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(r)
	}
	return strings.Trim(b.String(), "-")
}

func modelNamespace(model string) string {
	model = strings.TrimSpace(model)
	if slash := strings.LastIndexByte(model, '/'); slash > 0 {
		return model[:slash]
	}
	return ""
}

// stripModelSnapshot removes only a validated trailing release token. Numeric
// versions in the middle of a model ID, including claude-opus-4-6, remain
// untouched and therefore cannot be accidentally merged.
func stripModelSnapshot(model string) (string, bool) {
	model = strings.TrimSpace(model)
	for _, pattern := range []struct {
		parts int
		date  func([]string) bool
	}{
		{parts: 5, date: func(p []string) bool { return validCalendarDate(p[2], p[3], p[4]) }},
		{parts: 3, date: func(p []string) bool {
			raw := p[2]
			if len(raw) == 8 {
				return validCalendarDate(raw[:4], raw[4:6], raw[6:])
			}
			// Several provider catalogues (notably Volcengine) use YYMMDD
			// release suffixes such as `260215`. Restrict the year range to
			// contemporary/future releases so ordinary six-digit model IDs are
			// not mistaken for snapshots.
			return len(raw) == 6 && raw[:2] >= "20" && raw[:2] <= "39" && validCalendarDate("20"+raw[:2], raw[2:4], raw[4:])
		}},
		{parts: 4, date: func(p []string) bool { return validMonthDay(p[2], p[3]) }},
	} {
		parts := splitTrailingDate(model, pattern.parts)
		if len(parts) == pattern.parts && parts[1] != "" && pattern.date(parts) {
			return parts[0], true
		}
	}
	if marker, date, ok := splitTrailingPreviewDate(model); ok && validMonthDay(date[0], date[1]) {
		return marker, true
	}
	lower := strings.ToLower(model)
	for _, suffix := range []string{"-latest", "_latest", ".latest", "-stable", "_stable", ".stable"} {
		if strings.HasSuffix(lower, suffix) && len(model) > len(suffix) {
			return model[:len(model)-len(suffix)], true
		}
	}
	return model, false
}

// splitTrailingDate returns [base, separator, year, month, day] for a dashed
// date, or [base, separator, compactDate] for an eight-digit date. Keeping the
// parser local avoids broad regular expressions that can eat model versions.
func splitTrailingDate(model string, parts int) []string {
	if parts == 5 {
		for _, sep := range []string{"-", "_", "."} {
			idx := strings.LastIndex(model, sep)
			if idx < 0 || idx+1 >= len(model) {
				continue
			}
			day := model[idx+1:]
			beforeDay := model[:idx]
			idx2 := strings.LastIndex(beforeDay, sep)
			if idx2 < 0 {
				continue
			}
			month := beforeDay[idx2+1:]
			beforeMonth := beforeDay[:idx2]
			idx3 := strings.LastIndex(beforeMonth, sep)
			if idx3 < 0 {
				continue
			}
			year := beforeMonth[idx3+1:]
			base := beforeMonth[:idx3]
			if base != "" {
				return []string{base, sep, year, month, day}
			}
		}
		return nil
	}
	if parts == 4 {
		for _, sep := range []string{"-", "_", "."} {
			idx := strings.LastIndex(model, sep)
			if idx < 0 || idx+1 >= len(model) {
				continue
			}
			// Some catalogues encode a month/day release as one four-digit
			// token (for example deepseek-v4-pro-0813). Split that token
			// without treating a preceding version segment as the month.
			compact := model[idx+1:]
			if len(compact) == 4 && allDigits(compact) {
				return []string{model[:idx], sep, compact[:2], compact[2:]}
			}
			day := model[idx+1:]
			beforeDay := model[:idx]
			idx2 := strings.LastIndex(beforeDay, sep)
			if idx2 > 0 {
				month := beforeDay[idx2+1:]
				base := beforeDay[:idx2]
				if base != "" {
					return []string{base, sep, month, day}
				}
			}
		}
		return nil
	}
	for _, sep := range []string{"-", "_", "."} {
		idx := strings.LastIndex(model, sep)
		if idx > 0 {
			raw := model[idx+1:]
			if len(raw) == 8 || len(raw) == 6 {
				return []string{model[:idx], sep, raw}
			}
		}
	}
	return nil
}

func splitTrailingPreviewDate(model string) (string, [2]string, bool) {
	for _, marker := range []string{"-preview-", "_preview_", ".preview.", "-exp-", "_exp_", ".exp."} {
		idx := strings.LastIndex(strings.ToLower(model), marker)
		if idx < 0 {
			continue
		}
		rest := model[idx+len(marker):]
		for _, sep := range []string{"-", "_", "."} {
			pieces := strings.Split(rest, sep)
			if len(pieces) == 2 && len(pieces[0]) == 2 && len(pieces[1]) == 2 {
				return model[:idx] + model[idx:idx+len(marker)-1], [2]string{pieces[0], pieces[1]}, true
			}
		}
	}
	return "", [2]string{}, false
}

func validCalendarDate(year, month, day string) bool {
	if len(year) != 4 || len(month) != 2 || len(day) != 2 || !allDigits(year) || !allDigits(month) || !allDigits(day) {
		return false
	}
	y := atoiFixed(year)
	m := atoiFixed(month)
	d := atoiFixed(day)
	if y < 1900 || m < 1 || m > 12 || d < 1 || d > 31 {
		return false
	}
	max := [...]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[m]
	if m == 2 && (y%400 == 0 || (y%4 == 0 && y%100 != 0)) {
		max = 29
	}
	return d <= max
}

func validMonthDay(month, day string) bool {
	return validCalendarDate("2024", month, day)
}

func allDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func atoiFixed(value string) int {
	result := 0
	for _, r := range value {
		result = result*10 + int(r-'0')
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// priceLookupKey resolves a model alias against a price table. Multiple
// provider-qualified rows are safe to merge only when their billable fields
// and conditional variants are identical; otherwise the caller gets no price
// rather than an arbitrary provider's number.
func priceLookupKey(keys []string, requested string, entries map[string]*domain.PriceEntry, variants map[string][]*domain.PriceVariant) (string, bool) {
	return modelLookupKeyCoalesced(keys, requested, func(left, right string) bool {
		return priceEntriesEquivalent(entries[left], entries[right]) && priceVariantsEquivalent(variants[left], variants[right])
	})
}

func priceEntriesEquivalent(left, right *domain.PriceEntry) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Mode == right.Mode &&
		int64PointersEqual(left.InputPerM, right.InputPerM) &&
		int64PointersEqual(left.OutputPerM, right.OutputPerM) &&
		int64PointersEqual(left.CacheReadPerM, right.CacheReadPerM) &&
		int64PointersEqual(left.CacheWritePerM, right.CacheWritePerM) &&
		int64PointersEqual(left.PricePerCall, right.PricePerCall) &&
		int64PointersEqual(left.ImgInTokPerM, right.ImgInTokPerM) &&
		int64PointersEqual(left.ImgOutTokPerM, right.ImgOutTokPerM) &&
		int64PointersEqual(left.PricePerImage, right.PricePerImage)
}

func priceVariantsEquivalent(left, right []*domain.PriceVariant) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		a, b := left[i], right[i]
		if a == nil || b == nil {
			if a != b {
				return false
			}
			continue
		}
		if a.Seq != b.Seq ||
			!stringPointersEqual(a.ServiceTier, b.ServiceTier) ||
			!int64PointersEqual(a.CtxMin, b.CtxMin) ||
			!int64PointersEqual(a.CtxMax, b.CtxMax) ||
			!stringPointersEqual(a.TimeStart, b.TimeStart) ||
			!stringPointersEqual(a.TimeEnd, b.TimeEnd) ||
			!intPointersEqual(a.DowMask, b.DowMask) ||
			!intPointersEqual(a.MultBP, b.MultBP) ||
			!int64PointersEqual(a.SetInputPerM, b.SetInputPerM) ||
			!int64PointersEqual(a.SetOutputPerM, b.SetOutputPerM) ||
			!int64PointersEqual(a.SetCacheReadPerM, b.SetCacheReadPerM) ||
			!int64PointersEqual(a.SetCacheCreationPerM, b.SetCacheCreationPerM) ||
			!int64PointersEqual(a.SetPricePerCall, b.SetPricePerCall) ||
			!int64PointersEqual(a.SetImgInTokPerM, b.SetImgInTokPerM) ||
			!int64PointersEqual(a.SetImgOutTokPerM, b.SetImgOutTokPerM) ||
			!int64PointersEqual(a.SetPricePerImage, b.SetPricePerImage) {
			return false
		}
	}
	return true
}

func int64PointersEqual(left, right *int64) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func intPointersEqual(left, right *int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func stringPointersEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// sortedModelKeys provides stable iteration for tests and future callers that
// build a key slice from a map.
func sortedModelKeys[T any](table map[string]T) []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
