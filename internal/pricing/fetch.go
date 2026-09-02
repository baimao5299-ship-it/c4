// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

const FetchTimeout = 30 * time.Second
const maxPriceTableBytes = 64 << 20

type Fetcher interface {
	Fetch(ctx context.Context, sourceURL string) (*FetchResult, error)
}

type FetchResult struct {
	PriceEntries []*domain.PriceEntry
	Variants     []*domain.PriceVariant
	// Models contains every model key present in the source document, including
	// entries that have no billable price and are therefore counted in Skipped.
	// Keeping source keys separate from parsed price rows distinguishes a complete
	// document with unsupported rows from a legacy partial Fetcher result. Stale
	// price reconciliation itself uses SnapshotPriceModels below.
	Models  []string
	Skipped int
}

func NewFetcher(client *http.Client, log *logx.Logger) Fetcher {
	if client == nil {
		client = http.DefaultClient
	}
	return &httpFetcher{client: client, log: log, builtin: builtinOfficialPrices(log)}
}

type httpFetcher struct {
	client *http.Client
	log    *logx.Logger
	builtin *FetchResult
}

func (f *httpFetcher) Fetch(ctx context.Context, sourceURL string) (*FetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch %s: %w", sourceURL, err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing: fetch %s: status %d", sourceURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPriceTableBytes+1))
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch %s: read body: %w", sourceURL, err)
	}
	if len(data) > maxPriceTableBytes {
		return nil, fmt.Errorf("pricing: fetch %s: response exceeds %d bytes", sourceURL, maxPriceTableBytes)
	}
	result, err := Parse(data, f.log)
	if err != nil {
		return nil, err
	}
	return mergeWithBuiltinPrices(result, f.builtin), nil
}

type litellmEntry struct {
	InputCostPerToken                   *float64               `json:"input_cost_per_token"`
	OutputCostPerToken                  *float64               `json:"output_cost_per_token"`
	CacheReadInputTokenCost             *float64               `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost         *float64               `json:"cache_creation_input_token_cost"`
	InputCostPerTokenPriority           *float64               `json:"input_cost_per_token_priority"`
	OutputCostPerTokenPriority          *float64               `json:"output_cost_per_token_priority"`
	CacheReadInputTokenCostPriority     *float64               `json:"cache_read_input_token_cost_priority"`
	CacheCreationInputTokenCostPriority *float64               `json:"cache_creation_input_token_cost_priority"`
	InputCostPerTokenFlex               *float64               `json:"input_cost_per_token_flex"`
	OutputCostPerTokenFlex              *float64               `json:"output_cost_per_token_flex"`
	CacheReadInputTokenCostFlex         *float64               `json:"cache_read_input_token_cost_flex"`
	CacheCreationInputTokenCostFlex     *float64               `json:"cache_creation_input_token_cost_flex"`
	ProviderSpecificEntry               *providerSpecificEntry `json:"provider_specific_entry"`
	MaxInputTokens                      *float64               `json:"max_input_tokens"`
	MaxOutputTokens                     *float64               `json:"max_output_tokens"`
	Provider                            *string                `json:"litellm_provider"`
	Mode                                *string                `json:"mode"`
	SupportsPromptCaching               *bool                  `json:"supports_prompt_caching"`
	InputCostPerImageToken              *float64               `json:"input_cost_per_image_token"`
	OutputCostPerImageToken             *float64               `json:"output_cost_per_image_token"`
	InputCostPerImage                   *float64               `json:"input_cost_per_image"`
	OutputCostPerImage                  *float64               `json:"output_cost_per_image"`
	InputCostPerPixel                   *float64               `json:"input_cost_per_pixel"`
	OutputCostPerPixel                  *float64               `json:"output_cost_per_pixel"`
	InputCostPerCharacter               *float64               `json:"input_cost_per_character"`
	OutputCostPerCharacter              *float64               `json:"output_cost_per_character"`
	InputCostPerSecond                  *float64               `json:"input_cost_per_second"`
	OutputCostPerSecond                 *float64               `json:"output_cost_per_second"`
	InputCostPerAudioToken              *float64               `json:"input_cost_per_audio_token"`
	OutputCostPerAudioToken             *float64               `json:"output_cost_per_audio_token"`
	OutputCostPerReasoningToken         *float64               `json:"output_cost_per_reasoning_token"`
	InputCostPerQuery                   *float64               `json:"input_cost_per_query"`
	TieredPricing                       json.RawMessage        `json:"tiered_pricing"`
}

type providerSpecificEntry struct {
	Fast *float64 `json:"fast"`
}

func Parse(data []byte, log *logx.Logger) (*FetchResult, error) {
	// Price catalogues are published both as LiteLLM JSON and as the
	// provider-normalized CSV exports used by the admin tooling. Detect the
	// format from the first non-whitespace byte so one configured source can be
	// switched without a code or schema change.
	trimmed := strings.TrimSpace(string(data))
	if trimmed != "" && trimmed[0] != '{' && trimmed[0] != '[' {
		return ParseCSV(data, log)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pricing: parse price table: %w", err)
	}
	// Provider model identifiers never need surrounding whitespace. Normalize
	// them before parsing/persistence so a malformed source cannot create two
	// database rows that later collapse to one runtime lookup key. A collision
	// after trimming is rejected rather than resolved by map iteration order.
	normalized := make(map[string]json.RawMessage, len(raw))
	skipped := 0
	for sourceModel, entry := range raw {
		model := strings.TrimSpace(sourceModel)
		if model == "" {
			skipped++
			continue
		}
		if _, exists := normalized[model]; exists {
			return nil, fmt.Errorf("pricing: duplicate model after trimming: %q", model)
		}
		normalized[model] = entry
	}
	// JSON objects are unordered. Keep the result stable so sync previews,
	// deterministic tests, and UI progress do not reshuffle on every refresh.
	models := make([]string, 0, len(normalized))
	for model := range normalized {
		models = append(models, model)
	}
	sort.Strings(models)
	res := &FetchResult{
		PriceEntries: make([]*domain.PriceEntry, 0, len(normalized)),
		Variants:     make([]*domain.PriceVariant, 0),
		Models:       models,
		Skipped:      skipped,
	}
	for _, model := range models {
		entry := normalized[model]
		if pe2, ok2 := parseImagePriceEntry(model, entry); ok2 {
			res.PriceEntries = append(res.PriceEntries, pe2)
			continue
		}
		// LiteLLM function/search records commonly carry zero token-cost
		// placeholders alongside the authoritative per-query price. Parse these
		// rows before token pricing so a placeholder cannot hide the call rate.
		if pe3, ok3 := parseFunctionPriceEntry(model, entry); ok3 {
			res.PriceEntries = append(res.PriceEntries, pe3)
			continue
		}
		pe, vars, ok := parsePriceEntry(model, entry, log)
		if ok {
			applyConservativeModelFloor(pe, vars)
			res.PriceEntries = append(res.PriceEntries, pe)
			res.Variants = append(res.Variants, vars...)
			continue
		}
		res.Skipped++
	}
	return res, nil
}

// ParseCSV parses the normalized USD-per-million token price exports used by
// the project. The parser intentionally accepts several header spellings so
// both official-provider and LiteLLM CSV snapshots can be used as a price
// source. Blank optional fields are left nil; a row with at least one
// representable billable component is retained.
func ParseCSV(data []byte, _ *logx.Logger) (*FetchResult, error) {
	r := csv.NewReader(strings.NewReader(string(data)))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("pricing: parse csv header: %w", err)
	}
	if len(header) == 0 {
		return nil, fmt.Errorf("pricing: csv header is empty")
	}
	cols := make(map[string]int, len(header))
	for i, name := range header {
		name = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "\ufeff")))
		if name != "" {
			cols[name] = i
		}
	}
	modelCol := firstCSVColumn(cols, "model_id", "model_key", "model", "model_name")
	if modelCol < 0 {
		return nil, fmt.Errorf("pricing: csv model_id column is required")
	}
	result := &FetchResult{PriceEntries: make([]*domain.PriceEntry, 0), Models: make([]string, 0)}
	byModel := make(map[string]*domain.PriceEntry)
	providerByModel := make(map[string]string)
	for rowNo := 2; ; rowNo++ {
		row, readErr := r.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("pricing: parse csv row %d: %w", rowNo, readErr)
		}
		if modelCol >= len(row) {
			result.Skipped++
			continue
		}
		model := strings.TrimSpace(row[modelCol])
		if model == "" {
			result.Skipped++
			continue
		}
		result.Models = appendUniqueModel(result.Models, model)
		entry, provider, ok := parseCSVPriceRow(cols, row, model)
		if !ok {
			result.Skipped++
			continue
		}
		if previous, exists := byModel[model]; exists {
			if csvEntriesEquivalent(previous, entry) {
				// Keep a useful provider label when an otherwise identical model is
				// listed by more than one provider.
				if previous.Provider == nil && provider != "" {
					p := provider
					previous.Provider = &p
					providerByModel[model] = provider
				}
				continue
			}
			// A catalogue can contain both the provider's public rate and a
			// discounted/aggregated rate for the same model ID. Never let row order
			// or a provider label select the cheaper value: billing must not
			// undercharge when sources disagree. Merge every populated component by
			// maximum, while retaining the preferred official provider label for
			// diagnostics.
			oldProvider := providerByModel[model]
			merged := mergeCSVEntriesConservative(previous, entry)
			if csvProviderScore(model, provider) > csvProviderScore(model, oldProvider) ||
				(csvProviderScore(model, provider) == csvProviderScore(model, oldProvider) && strings.ToLower(provider) < strings.ToLower(oldProvider)) {
				merged.Provider = csvStringPtr(provider)
				providerByModel[model] = provider
			}
			byModel[model] = merged
			continue
		}
		byModel[model] = entry
		providerByModel[model] = provider
	}
	sort.Strings(result.Models)
	models := make([]string, 0, len(byModel))
	for model := range byModel {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		result.PriceEntries = append(result.PriceEntries, byModel[model])
	}
	return result, nil
}

func mergeCSVEntriesConservative(left, right *domain.PriceEntry) *domain.PriceEntry {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	out := *left
	// A mode conflict is resolved in favour of the row that carries the
	// billable component. CSV exports normally agree on mode; this fallback
	// keeps a malformed metadata field from erasing a usable price.
	if out.Mode == "" && right.Mode != "" {
		out.Mode = right.Mode
	}
	out.InputPerM = maxInt64Ptr(left.InputPerM, right.InputPerM)
	out.OutputPerM = maxInt64Ptr(left.OutputPerM, right.OutputPerM)
	out.CacheReadPerM = maxInt64Ptr(left.CacheReadPerM, right.CacheReadPerM)
	out.CacheWritePerM = maxInt64Ptr(left.CacheWritePerM, right.CacheWritePerM)
	out.PricePerCall = maxInt64Ptr(left.PricePerCall, right.PricePerCall)
	out.ImgInTokPerM = maxInt64Ptr(left.ImgInTokPerM, right.ImgInTokPerM)
	out.ImgOutTokPerM = maxInt64Ptr(left.ImgOutTokPerM, right.ImgOutTokPerM)
	out.PricePerImage = maxInt64Ptr(left.PricePerImage, right.PricePerImage)
	if out.MaxInputTokens == nil || (right.MaxInputTokens != nil && *right.MaxInputTokens > *out.MaxInputTokens) {
		out.MaxInputTokens = right.MaxInputTokens
	}
	return &out
}

func maxInt64Ptr(left, right *int64) *int64 {
	if left == nil {
		return right
	}
	if right == nil || *left >= *right {
		return left
	}
	return right
}

func firstCSVColumn(cols map[string]int, names ...string) int {
	for _, name := range names {
		if i, ok := cols[name]; ok {
			return i
		}
	}
	return -1
}

func csvValue(cols map[string]int, row []string, names ...string) string {
	if i := firstCSVColumn(cols, names...); i >= 0 && i < len(row) {
		return strings.TrimSpace(row[i])
	}
	return ""
}

func parseCSVFloat(value string) (*float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "null") || value == "-" {
		return nil, true
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil || !validCost(&v) {
		return nil, false
	}
	return &v, true
}

func parseCSVPriceRow(cols map[string]int, row []string, model string) (*domain.PriceEntry, string, bool) {
	provider := csvValue(cols, row, "provider_id", "provider", "litellm_provider")
	mode := strings.ToLower(csvValue(cols, row, "mode"))
	if mode == "search" || mode == "rerank" || mode == "vector_store" {
		price, ok := parseCSVFloat(csvValue(cols, row, "input_usd_per_query", "input_cost_per_query"))
		if !ok || price == nil {
			return nil, provider, false
		}
		v, ok := toMilliCentsPerCall(*price)
		if !ok {
			return nil, provider, false
		}
		return &domain.PriceEntry{Model: model, Mode: domain.PriceModeCall, PricePerCall: &v, Provider: csvStringPtr(provider), Source: domain.PricingSourceLitellm}, provider, true
	}
	input, okIn := parseCSVFloat(csvValue(cols, row, "input_usd_per_1m"))
	output, okOut := parseCSVFloat(csvValue(cols, row, "output_usd_per_1m"))
	cacheRead, okRead := parseCSVFloat(csvValue(cols, row, "cache_read_usd_per_1m"))
	cacheWrite, okWrite := parseCSVFloat(csvValue(cols, row, "cache_write_usd_per_1m"))
	if !okIn || !okOut || !okRead || !okWrite {
		return nil, provider, false
	}
	// Image-mode exports use image-token columns rather than text-token
	// columns. Keep those rows billable as well; some feeds omit mode while
	// still exposing the image-specific fields, so the presence of a value is
	// accepted as an image row.
	imageInput, imageInputOK := parseCSVFloat(csvValue(cols, row, "input_image_usd_per_1m", "input_cost_per_image_token"))
	imageOutput, imageOutputOK := parseCSVFloat(csvValue(cols, row, "output_image_usd_per_1m", "output_cost_per_image_token"))
	perImage, perImageOK := parseCSVFloat(csvValue(cols, row, "price_per_image", "input_usd_per_image", "output_usd_per_image"))
	if !imageInputOK || !imageOutputOK || !perImageOK {
		return nil, provider, false
	}
	if (mode == "image_generation" || mode == "image_edit") ||
		(input == nil && output == nil && cacheRead == nil && cacheWrite == nil && (imageInput != nil || imageOutput != nil || perImage != nil)) {
		entry := &domain.PriceEntry{Model: model, Mode: domain.PriceModeImage, Provider: csvStringPtr(provider), Source: domain.PricingSourceLitellm}
		if imageInput != nil {
			v, ok := csvUSDPerMillion(*imageInput)
			if !ok {
				return nil, provider, false
			}
			entry.ImgInTokPerM = &v
		}
		if imageOutput != nil {
			v, ok := csvUSDPerMillion(*imageOutput)
			if !ok {
				return nil, provider, false
			}
			entry.ImgOutTokPerM = &v
		}
		if perImage != nil {
			v, ok := toMilliCentsPerImage(*perImage)
			if !ok {
				return nil, provider, false
			}
			entry.PricePerImage = &v
		}
		if entry.ImgInTokPerM == nil && entry.ImgOutTokPerM == nil && entry.PricePerImage == nil {
			return nil, provider, false
		}
		return entry, provider, true
	}
	entry := &domain.PriceEntry{Model: model, Mode: domain.PriceModeToken, Provider: csvStringPtr(provider), Source: domain.PricingSourceLitellm}
	if input != nil {
		v, ok := csvUSDPerMillion(*input)
		if !ok { return nil, provider, false }
		entry.InputPerM = &v
	}
	if output != nil {
		v, ok := csvUSDPerMillion(*output)
		if !ok { return nil, provider, false }
		entry.OutputPerM = &v
	}
	if cacheRead != nil {
		v, ok := csvUSDPerMillion(*cacheRead)
		if !ok { return nil, provider, false }
		entry.CacheReadPerM = &v
	}
	if cacheWrite != nil {
		v, ok := csvUSDPerMillion(*cacheWrite)
		if !ok { return nil, provider, false }
		entry.CacheWritePerM = &v
	}
	if entry.InputPerM == nil && entry.OutputPerM == nil && entry.CacheReadPerM == nil && entry.CacheWritePerM == nil {
		return nil, provider, false
	}
	if context := csvValue(cols, row, "context_window_tokens", "context_tokens", "max_input_tokens"); context != "" {
		if v, err := strconv.ParseInt(context, 10, 64); err == nil && v > 0 {
			entry.MaxInputTokens = &v
		}
	}
	return entry, provider, true
}

func csvUSDPerMillion(value float64) (int64, bool) {
	return scaledPositiveInt64(value, 1e5)
}

func csvStringPtr(value string) *string {
	if value == "" { return nil }
	return &value
}

func appendUniqueModel(models []string, model string) []string {
	for _, existing := range models {
		if existing == model { return models }
	}
	return append(models, model)
}

func csvEntriesEquivalent(left, right *domain.PriceEntry) bool {
	if left == nil || right == nil { return left == right }
	return left.Mode == right.Mode &&
		int64PtrEqual(left.InputPerM, right.InputPerM) && int64PtrEqual(left.OutputPerM, right.OutputPerM) &&
		int64PtrEqual(left.CacheReadPerM, right.CacheReadPerM) && int64PtrEqual(left.CacheWritePerM, right.CacheWritePerM) &&
		int64PtrEqual(left.PricePerCall, right.PricePerCall) && int64PtrEqual(left.ImgInTokPerM, right.ImgInTokPerM) &&
		int64PtrEqual(left.ImgOutTokPerM, right.ImgOutTokPerM) && int64PtrEqual(left.PricePerImage, right.PricePerImage)
}

func int64PtrEqual(left, right *int64) bool {
	if left == nil || right == nil { return left == right }
	return *left == *right
}

func csvProviderScore(model, provider string) int {
	p := strings.ToLower(strings.TrimSpace(provider))
	m := strings.ToLower(model)
	if slash := strings.LastIndexByte(m, '/'); slash >= 0 && slash+1 < len(m) {
		m = m[slash+1:]
	}
	switch {
	case strings.HasPrefix(m, "claude") && strings.Contains(p, "anthropic"):
		return 100
	case strings.HasPrefix(m, "gpt-") && strings.Contains(p, "openai"):
		return 100
	case strings.HasPrefix(m, "gemini") && strings.Contains(p, "google"):
		return 100
	case (strings.Contains(m, "kimi") || strings.Contains(m, "moonshot")) && strings.Contains(p, "moonshot"):
		return 100
	case strings.HasPrefix(m, "glm") && (strings.Contains(p, "zhipu") || strings.Contains(p, "z.ai")):
		return 100
	case p == "anthropic" || p == "openai" || p == "google":
		return 80
	case p != "":
		return 20
	default:
		return 0
	}
}

// applyConservativeModelFloor keeps the operator-confirmed public price floor
// for the current GPT-5.6 catalogue entries. A provider feed that temporarily
// reports a lower value must not silently reduce customer charges; the floor
// is applied to the base row and every conditional output variant.
func applyConservativeModelFloor(pe *domain.PriceEntry, variants []*domain.PriceVariant) {
	if pe == nil {
		return
	}
	floor, ok := map[string]int64{"gpt-5.6": 3_000_000, "gpt-5.6-sol": 3_000_000}[strings.TrimSpace(pe.Model)]
	if !ok {
		return
	}
	if pe.OutputPerM == nil || *pe.OutputPerM < floor {
		v := floor
		pe.OutputPerM = &v
	}
	for _, variant := range variants {
		if variant == nil || variant.SetOutputPerM == nil || *variant.SetOutputPerM >= floor {
			continue
		}
		v := floor
		variant.SetOutputPerM = &v
	}
}

// SnapshotPriceModels returns the normalized model set whose price rows can be
// represented and billed by C4. The boolean reports whether that set is an
// authoritative snapshot: Parse always supplies Models, while legacy Fetcher
// implementations without Models are authoritative only when they did not
// report skipped rows.
//
// Reconciliation must use this set rather than every source key. Keeping a
// source key that no longer has a parseable price would otherwise preserve its
// previous database row forever and continue billing at a stale price.
func SnapshotPriceModels(res *FetchResult) ([]string, bool) {
	if res == nil {
		return nil, false
	}
	authoritative := res.Models != nil || res.Skipped == 0
	if !authoritative {
		return nil, false
	}
	seen := make(map[string]struct{}, len(res.PriceEntries))
	models := make([]string, 0, len(res.PriceEntries))
	for _, entry := range res.PriceEntries {
		if entry == nil {
			continue
		}
		model := strings.TrimSpace(entry.Model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	sort.Strings(models)
	return models, true
}

func parsePriceEntry(model string, raw json.RawMessage, log *logx.Logger) (*domain.PriceEntry, []*domain.PriceVariant, bool) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, nil, false
	}
	tiers := parseTieredPricing(e.TieredPricing)
	// A function entry with an explicit query field (top-level or in a tier) is
	// owned by the call parser. If that parser rejected the field (for example a
	// negative or unrepresentable value), do not rescue the row through zero
	// token placeholders and publish a misleading token price.
	if isFunctionPriceMode(e.Mode) && (e.InputCostPerQuery != nil || hasTieredQueryPrice(tiers)) {
		return nil, nil, false
	}
	// Nil means that a provider omitted a component. A present negative or
	// non-finite value is malformed and must not be rescued by tier promotion.
	if !validOptionalCost(e.InputCostPerToken) || !validOptionalCost(e.OutputCostPerToken) {
		return nil, nil, false
	}
	for _, value := range []*float64{
		e.CacheReadInputTokenCost, e.CacheCreationInputTokenCost,
		e.InputCostPerTokenPriority, e.OutputCostPerTokenPriority,
		e.CacheReadInputTokenCostPriority, e.CacheCreationInputTokenCostPriority,
		e.InputCostPerTokenFlex, e.OutputCostPerTokenFlex,
		e.CacheReadInputTokenCostFlex, e.CacheCreationInputTokenCostFlex,
	} {
		if !validOptionalCost(value) {
			return nil, nil, false
		}
	}
	if !validTokenTiers(tiers) {
		return nil, nil, false
	}
	// The persisted grid cannot represent a positive value below one unit. A
	// tiny optional component is omitted (it must not become an apparent free
	// tier), while an overflowing base input/output price still rejects the row.
	var representable bool
	if e.InputCostPerToken, representable = normalizeBaseTokenCost(e.InputCostPerToken); !representable {
		return nil, nil, false
	}
	if e.OutputCostPerToken, representable = normalizeBaseTokenCost(e.OutputCostPerToken); !representable {
		return nil, nil, false
	}
	e.CacheReadInputTokenCost = normalizeOptionalTokenCost(e.CacheReadInputTokenCost)
	e.CacheCreationInputTokenCost = normalizeOptionalTokenCost(e.CacheCreationInputTokenCost)
	e.InputCostPerTokenPriority = normalizeOptionalTokenCost(e.InputCostPerTokenPriority)
	e.OutputCostPerTokenPriority = normalizeOptionalTokenCost(e.OutputCostPerTokenPriority)
	e.CacheReadInputTokenCostPriority = normalizeOptionalTokenCost(e.CacheReadInputTokenCostPriority)
	e.CacheCreationInputTokenCostPriority = normalizeOptionalTokenCost(e.CacheCreationInputTokenCostPriority)
	e.InputCostPerTokenFlex = normalizeOptionalTokenCost(e.InputCostPerTokenFlex)
	e.OutputCostPerTokenFlex = normalizeOptionalTokenCost(e.OutputCostPerTokenFlex)
	e.CacheReadInputTokenCostFlex = normalizeOptionalTokenCost(e.CacheReadInputTokenCostFlex)
	e.CacheCreationInputTokenCostFlex = normalizeOptionalTokenCost(e.CacheCreationInputTokenCostFlex)
	for i := range tiers {
		tiers[i].InputCostPerToken = normalizeOptionalTokenCost(tiers[i].InputCostPerToken)
		tiers[i].OutputCostPerToken = normalizeOptionalTokenCost(tiers[i].OutputCostPerToken)
		tiers[i].CacheReadInputTokenCost = normalizeOptionalTokenCost(tiers[i].CacheReadInputTokenCost)
		tiers[i].CacheCreationInputTokenCost = normalizeOptionalTokenCost(tiers[i].CacheCreationInputTokenCost)
	}
	// Some providers (notably DashScope and Volcengine) publish no top-level
	// token prices and put every tier in `tiered_pricing`. Promote the first
	// (lowest context) tier to the base row; the remaining tiers become
	// context-bound variants below.
	if base, ok := firstTokenTier(tiers); ok {
		// Fill only omitted fields. A malformed top-level value must still be
		// rejected below instead of being silently replaced by a tier value.
		if e.InputCostPerToken == nil && pricePerMillion(base.InputCostPerToken) != nil {
			e.InputCostPerToken = cloneFloat(base.InputCostPerToken)
		}
		if e.OutputCostPerToken == nil && pricePerMillion(base.OutputCostPerToken) != nil {
			e.OutputCostPerToken = cloneFloat(base.OutputCostPerToken)
		}
		if e.CacheReadInputTokenCost == nil {
			e.CacheReadInputTokenCost = cloneFloat(base.CacheReadInputTokenCost)
		}
		if e.CacheCreationInputTokenCost == nil {
			e.CacheCreationInputTokenCost = cloneFloat(base.CacheCreationInputTokenCost)
		}
	}
	// LiteLLM has several token-priced modes in addition to chat/responses.
	// Completion and realtime entries are valid billing rows too, while modes
	// such as embedding can legitimately expose only one token side. A missing
	// mode is accepted when a token component exists; entries that only expose
	// image/call pricing are handled by their dedicated parsers.
	if !isTokenPriceMode(e.Mode, e.InputCostPerToken, e.OutputCostPerToken) {
		return nil, nil, false
	}
	// A present negative/non-finite component is malformed. Treating it as
	// absent would make a typo look like a valid one-sided price. Nil is the
	// only accepted way to omit a side.
	if (e.InputCostPerToken != nil && !validCost(e.InputCostPerToken)) || (e.OutputCostPerToken != nil && !validCost(e.OutputCostPerToken)) {
		return nil, nil, false
	}
	// LiteLLM uses an explicit numeric zero for free input/output. At least one
	// side is required, while a missing side remains nil because a number must
	// never be fabricated for providers that bill only input or only output.
	if !validCost(e.InputCostPerToken) && !validCost(e.OutputCostPerToken) {
		return nil, nil, false
	}
	pe := &domain.PriceEntry{
		Model:  model,
		Mode:   domain.PriceModeToken,
		Source: domain.PricingSourceLitellm,
		Raw:    raw,
	}
	if validCost(e.InputCostPerToken) {
		in, ok := toMilliCentsPerMillion(*e.InputCostPerToken)
		if !ok {
			return nil, nil, false
		}
		pe.InputPerM = &in
	}
	if validCost(e.OutputCostPerToken) {
		out, ok := toMilliCentsPerMillion(*e.OutputCostPerToken)
		if !ok {
			return nil, nil, false
		}
		pe.OutputPerM = &out
	}
	if v := cacheCost(e.CacheReadInputTokenCost); v != nil {
		pe.CacheReadPerM = v
	}
	if v := cacheCost(e.CacheCreationInputTokenCost); v != nil {
		pe.CacheWritePerM = v
	}
	if t := windowTokens(e.MaxInputTokens); t > 0 {
		pe.MaxInputTokens = &t
	}
	if t := windowTokens(e.MaxOutputTokens); t > 0 {
		pe.MaxOutputTokens = &t
	}
	pe.Provider = e.Provider
	pe.SupportsPromptCaching = e.SupportsPromptCaching
	var vars []*domain.PriceVariant
	seq := 1
	// Preserve provider service-tier prices, including cache components. A
	// missing component intentionally remains nil so the resolver falls back to
	// the base price for that component.
	if e.InputCostPerTokenPriority != nil || e.OutputCostPerTokenPriority != nil || e.CacheReadInputTokenCostPriority != nil || e.CacheCreationInputTokenCostPriority != nil {
		if validCost(e.InputCostPerTokenPriority) || validCost(e.OutputCostPerTokenPriority) || validCost(e.CacheReadInputTokenCostPriority) || validCost(e.CacheCreationInputTokenCostPriority) {
			st := "priority"
			v := &domain.PriceVariant{Model: model, Seq: seq, ServiceTier: &st}
			setTokenVariantPrices(v, e.InputCostPerTokenPriority, e.OutputCostPerTokenPriority, e.CacheReadInputTokenCostPriority, e.CacheCreationInputTokenCostPriority)
			if variantHasPriceEffect(v) {
				vars = append(vars, v)
				seq++
			}
		}
	}
	if e.InputCostPerTokenFlex != nil || e.OutputCostPerTokenFlex != nil || e.CacheReadInputTokenCostFlex != nil || e.CacheCreationInputTokenCostFlex != nil {
		if validCost(e.InputCostPerTokenFlex) || validCost(e.OutputCostPerTokenFlex) || validCost(e.CacheReadInputTokenCostFlex) || validCost(e.CacheCreationInputTokenCostFlex) {
			st := "flex"
			v := &domain.PriceVariant{Model: model, Seq: seq, ServiceTier: &st}
			setTokenVariantPrices(v, e.InputCostPerTokenFlex, e.OutputCostPerTokenFlex, e.CacheReadInputTokenCostFlex, e.CacheCreationInputTokenCostFlex)
			if variantHasPriceEffect(v) {
				vars = append(vars, v)
				seq++
			}
		}
	}
	vars = appendTieredVariants(model, vars, tiers, &seq)
	above := extractAboveVariants(model, raw)
	aboveKeys := make([]aboveKey, 0, len(above))
	for key := range above {
		aboveKeys = append(aboveKeys, key)
	}
	sort.Slice(aboveKeys, func(i, j int) bool {
		if aboveKeys[i].threshold != aboveKeys[j].threshold {
			// More specific thresholds must win before broader ones. The
			// resolver stops at the first matching variant.
			return aboveKeys[i].threshold > aboveKeys[j].threshold
		}
		// At the same context threshold, a service-tier-specific row must win
		// over the generic row because the resolver stops at the first match.
		if (aboveKeys[i].tier != "") != (aboveKeys[j].tier != "") {
			return aboveKeys[i].tier != ""
		}
		return aboveKeys[i].tier < aboveKeys[j].tier
	})
	for _, key := range aboveKeys {
		a := above[key]
		ctxMin := key.threshold
		v := &domain.PriceVariant{Model: model, Seq: seq, CtxMin: &ctxMin}
		if key.tier != "" {
			v.ServiceTier = cloneStringPtr(key.tier)
		}
		v.SetInputPerM = a.input
		v.SetOutputPerM = a.output
		v.SetCacheReadPerM = a.cacheRead
		v.SetCacheCreationPerM = a.cacheWrite
		if variantHasPriceEffect(v) {
			vars = append(vars, v)
			seq++
		}
	}
	if e.ProviderSpecificEntry != nil && e.ProviderSpecificEntry.Fast != nil {
		if m, ok := providerFastMultiplierBP(e.ProviderSpecificEntry.Fast); ok {
			st := "fast"
			vars = append(vars, &domain.PriceVariant{Model: model, Seq: seq, ServiceTier: &st, MultBP: &m})
			seq++
		}
	}
	vars = orderGeneratedVariants(vars)
	return pe, vars, true
}

func providerFastMultiplierBP(value *float64) (int, bool) {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value <= 0 || *value > 10 {
		return 0, false
	}
	scaled := *value * 1e4
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) {
		return 0, false
	}
	rounded := math.Round(scaled)
	// A positive provider multiplier that rounds to zero is not an explicit
	// free tier. Dropping the malformed optional variant safely falls back to
	// the base price instead of turning fast requests into free traffic.
	if rounded <= 0 || rounded > 100000 {
		return 0, false
	}
	return int(rounded), true
}

// orderGeneratedVariants sets the resolver order for rows generated from the
// provider price table. ResolveEntryPrices deliberately stops at the first
// matching row, so a conditional row must precede a less-specific fallback:
// service-tier + context > service-tier only > context only > unconditional.
// Within a context group, the highest lower bound wins; this keeps overlapping
// above_* rows deterministic and makes the more specific row reachable.
func orderGeneratedVariants(vars []*domain.PriceVariant) []*domain.PriceVariant {
	if len(vars) < 2 {
		if len(vars) == 1 && vars[0] != nil {
			vars[0].Seq = 1
		}
		return vars
	}
	sort.SliceStable(vars, func(i, j int) bool {
		a, b := vars[i], vars[j]
		ra, rb := variantSpecificityRank(a), variantSpecificityRank(b)
		if ra != rb {
			return ra < rb
		}
		if variantHasContext(a) && variantHasContext(b) {
			amin, bmin := variantContextMin(a), variantContextMin(b)
			if amin != bmin {
				return amin > bmin
			}
			// For the same lower bound, a bounded range is more specific than
			// an unbounded one; then prefer the narrower upper bound.
			amax, bmax := variantContextMax(a), variantContextMax(b)
			if amax != bmax {
				return amax < bmax
			}
		}
		if variantHasService(a) && variantHasService(b) {
			as, bs := *a.ServiceTier, *b.ServiceTier
			if as != bs {
				return as < bs
			}
		}
		return a.Seq < b.Seq
	})
	for i, v := range vars {
		if v != nil {
			v.Seq = i + 1
		}
	}
	return vars
}

func variantHasService(v *domain.PriceVariant) bool {
	return v != nil && v.ServiceTier != nil && strings.TrimSpace(*v.ServiceTier) != ""
}

func variantHasContext(v *domain.PriceVariant) bool {
	return v != nil && (v.CtxMin != nil || v.CtxMax != nil)
}

func variantSpecificityRank(v *domain.PriceVariant) int {
	service, context := variantHasService(v), variantHasContext(v)
	switch {
	case service && context:
		return 0
	case service:
		return 1
	case context:
		return 2
	default:
		return 3
	}
}

func variantContextMin(v *domain.PriceVariant) int64 {
	if v == nil || v.CtxMin == nil {
		return 0
	}
	return *v.CtxMin
}

func variantContextMax(v *domain.PriceVariant) int64 {
	if v == nil || v.CtxMax == nil {
		return math.MaxInt64
	}
	return *v.CtxMax
}

func isTokenPriceMode(mode *string, input, output *float64) bool {
	if !validCost(input) && !validCost(output) {
		return false
	}
	if mode == nil {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(*mode)) {
	case "chat", "responses", "completion", "text_completion", "realtime", "audio_speech", "audio_transcription", "embedding", "moderation", "rerank", "ocr":
		return true
	default:
		return false
	}
}

// tieredPrice is the subset of LiteLLM's tiered_pricing item that can be
// represented by the gateway's fixed-point price matrix. Other provider
// metadata remains in PriceEntry.Raw and is intentionally not guessed.
type tieredPrice struct {
	InputCostPerToken           *float64  `json:"input_cost_per_token"`
	OutputCostPerToken          *float64  `json:"output_cost_per_token"`
	CacheReadInputTokenCost     *float64  `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost *float64  `json:"cache_creation_input_token_cost"`
	InputCostPerQuery           *float64  `json:"input_cost_per_query"`
	Range                       []float64 `json:"range"`
	MaxResultsRange             []float64 `json:"max_results_range"`
}

func parseTieredPricing(raw json.RawMessage) []tieredPrice {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil
	}
	out := make([]tieredPrice, 0, len(items))
	for _, item := range items {
		var tier tieredPrice
		if err := json.Unmarshal(item, &tier); err != nil || string(item) == "null" {
			continue
		}
		out = append(out, tier)
	}
	return out
}

func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

func cloneStringPtr(v string) *string {
	return &v
}

func firstTokenTier(tiers []tieredPrice) (tieredPrice, bool) {
	best := tieredPrice{}
	bestLower := int64(math.MaxInt64)
	found := false
	for _, tier := range tiers {
		lower, _, ok := tierBounds(tier)
		if !ok {
			continue
		}
		// A tier that starts above zero is not a base price. Promoting it would
		// charge short requests at a high-context rate (and can also fabricate a
		// missing input/output component). Keep it as a conditional variant;
		// only a zero-bound tier may seed the unconditional row.
		if lower != 0 {
			continue
		}
		// Use the same fixed-point conversion as the persisted row. This
		// prevents a tiny positive value that rounds below one grid unit from
		// poisoning an otherwise usable later tier.
		if pricePerMillion(tier.InputCostPerToken) == nil && pricePerMillion(tier.OutputCostPerToken) == nil {
			continue
		}
		if !found || lower < bestLower {
			best, bestLower, found = tier, lower, true
		}
	}
	return best, found
}

func tierBounds(tier tieredPrice) (lower, upper int64, ok bool) {
	if len(tier.Range) == 0 {
		return 0, 0, true
	}
	if len(tier.Range) > 2 {
		return 0, 0, false
	}
	lower, ok = tokenBound(tier.Range[0])
	if !ok {
		return 0, 0, false
	}
	if len(tier.Range) == 1 {
		return lower, 0, true
	}
	upper, ok = tokenBound(tier.Range[1])
	if !ok || upper <= lower {
		return 0, 0, false
	}
	return lower, upper, true
}

func validTierComponent(v *float64) bool {
	// Range validation rejects malformed negative/non-finite values. Positive
	// values below the persisted precision are sanitized per component below so
	// one optional tier field cannot discard an otherwise usable model.
	return v == nil || validCost(v)
}

func validTokenTiers(tiers []tieredPrice) bool {
	for _, tier := range tiers {
		hasTokenComponent := tier.InputCostPerToken != nil || tier.OutputCostPerToken != nil || tier.CacheReadInputTokenCost != nil || tier.CacheCreationInputTokenCost != nil
		if !hasTokenComponent {
			continue
		}
		if !validTierComponent(tier.InputCostPerToken) || !validTierComponent(tier.OutputCostPerToken) || !validTierComponent(tier.CacheReadInputTokenCost) || !validTierComponent(tier.CacheCreationInputTokenCost) {
			return false
		}
		if _, _, ok := tierBounds(tier); !ok {
			return false
		}
	}
	return true
}

func tokenBound(v float64) (int64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= float64(uint64(1)<<63) {
		return 0, false
	}
	rounded := math.Round(v)
	if math.Abs(v-rounded) > 1e-6 || rounded >= float64(uint64(1)<<63) {
		return 0, false
	}
	return int64(rounded), true
}

func pricePerMillion(v *float64) *int64 {
	if !validCost(v) {
		return nil
	}
	n, ok := toMilliCentsPerMillion(*v)
	if !ok {
		return nil
	}
	return &n
}

// normalizeBaseTokenCost keeps the base input/output pair authoritative. Any
// positive value that cannot be represented is rejected rather than silently
// omitted, because dropping an explicitly supplied base component would turn
// a two-sided price into an undercharging one-sided row.
func normalizeBaseTokenCost(v *float64) (*float64, bool) {
	if v == nil {
		return nil, true
	}
	if !validCost(v) {
		return nil, false
	}
	if *v == 0 {
		return cloneFloat(v), true
	}
	scaled := *v * 1e11
	limit := float64(uint64(1) << 63)
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled >= limit {
		return nil, false
	}
	if math.Round(scaled) <= 0 {
		return nil, false
	}
	return cloneFloat(v), true
}

// normalizeOptionalTokenCost drops only values that cannot be represented by
// the fixed-point price grid. Malformed negatives/non-finite values are
// rejected by the caller's validation pass before this helper is reached.
func normalizeOptionalTokenCost(v *float64) *float64 {
	if v == nil || !validCost(v) || !representablePerMillion(v) {
		return nil
	}
	return cloneFloat(v)
}

func setTokenVariantPrices(v *domain.PriceVariant, input, output, cacheRead, cacheWrite *float64) {
	if v == nil {
		return
	}
	v.SetInputPerM = pricePerMillion(input)
	v.SetOutputPerM = pricePerMillion(output)
	v.SetCacheReadPerM = pricePerMillion(cacheRead)
	v.SetCacheCreationPerM = pricePerMillion(cacheWrite)
}

func variantHasPriceEffect(v *domain.PriceVariant) bool {
	return v != nil && (v.MultBP != nil || v.SetInputPerM != nil || v.SetOutputPerM != nil || v.SetCacheReadPerM != nil || v.SetCacheCreationPerM != nil || v.SetPricePerCall != nil || v.SetImgInTokPerM != nil || v.SetImgOutTokPerM != nil || v.SetPricePerImage != nil)
}

func appendTieredVariants(model string, variants []*domain.PriceVariant, tiers []tieredPrice, seq *int) []*domain.PriceVariant {
	if len(tiers) == 0 || seq == nil {
		return variants
	}
	ordered := append([]tieredPrice(nil), tiers...)
	sort.SliceStable(ordered, func(i, j int) bool {
		li, iok := int64(0), true
		lj, jok := int64(0), true
		if len(ordered[i].Range) > 0 {
			li, iok = tokenBound(ordered[i].Range[0])
		}
		if len(ordered[j].Range) > 0 {
			lj, jok = tokenBound(ordered[j].Range[0])
		}
		if iok != jok {
			return iok
		}
		return li < lj
	})
	for _, tier := range ordered {
		if !validTierComponent(tier.InputCostPerToken) || !validTierComponent(tier.OutputCostPerToken) || !validTierComponent(tier.CacheReadInputTokenCost) || !validTierComponent(tier.CacheCreationInputTokenCost) {
			continue
		}
		if tier.InputCostPerToken == nil && tier.OutputCostPerToken == nil && tier.CacheReadInputTokenCost == nil && tier.CacheCreationInputTokenCost == nil {
			continue
		}
		lower, upper, ok := tierBounds(tier)
		if !ok {
			continue
		}
		if lower == 0 {
			// A zero lower bound is represented by the base row. Treat every
			// additional zero-bound item as duplicate metadata rather than a
			// catch-all variant that would override the base price.
			continue
		}
		ctxMin := lower
		v := &domain.PriceVariant{Model: model, Seq: *seq, CtxMin: &ctxMin}
		if upper > lower {
			v.CtxMax = &upper
		}
		setTokenVariantPrices(v, tier.InputCostPerToken, tier.OutputCostPerToken, tier.CacheReadInputTokenCost, tier.CacheCreationInputTokenCost)
		if variantHasPriceEffect(v) {
			variants = append(variants, v)
			(*seq)++
		}
	}
	return variants
}

type aboveKey struct {
	threshold int64
	tier      string
}

type abovePrice struct {
	input      *int64
	output     *int64
	cacheRead  *int64
	cacheWrite *int64
}

// extractAboveVariants handles LiteLLM's context tiers, including priority and
// flex suffixes and cache prices. A tier is retained when any representable
// component is present; resolver fallback supplies omitted components.
func extractAboveVariants(_ string, raw json.RawMessage) map[aboveKey]abovePrice {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	out := make(map[aboveKey]abovePrice)
	for key, value := range fields {
		component, rest, ok := aboveComponent(key)
		if !ok {
			continue
		}
		tier := ""
		for _, suffix := range []string{"_priority", "_flex"} {
			if strings.HasSuffix(rest, suffix) {
				tier = suffix[1:]
				rest = strings.TrimSuffix(rest, suffix)
				break
			}
		}
		const suffix = "k_tokens"
		if !strings.HasSuffix(rest, suffix) {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(rest, suffix), 10, 64)
		if err != nil || n <= 0 || n > math.MaxInt64/1000 {
			continue
		}
		var f float64
		if err := json.Unmarshal(value, &f); err != nil || !validCost(&f) {
			continue
		}
		milli, ok := toMilliCentsPerMillion(f)
		if !ok {
			continue
		}
		k := aboveKey{threshold: n * 1000, tier: tier}
		a := out[k]
		ptr := &milli
		switch component {
		case "input":
			a.input = ptr
		case "output":
			a.output = ptr
		case "cache_read":
			a.cacheRead = ptr
		case "cache_write":
			a.cacheWrite = ptr
		}
		out[k] = a
	}
	return out
}

func aboveComponent(key string) (component, rest string, ok bool) {
	for prefix, name := range map[string]string{
		"input_cost_per_token_above_":            "input",
		"output_cost_per_token_above_":           "output",
		"cache_read_input_token_cost_above_":     "cache_read",
		"cache_creation_input_token_cost_above_": "cache_write",
	} {
		if strings.HasPrefix(key, prefix) {
			return name, strings.TrimPrefix(key, prefix), true
		}
	}
	return "", "", false
}

func parseImagePriceEntry(model string, raw json.RawMessage) (*domain.PriceEntry, bool) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false
	}
	for _, value := range []*float64{
		e.InputCostPerToken, e.OutputCostPerToken,
		e.CacheReadInputTokenCost, e.CacheCreationInputTokenCost,
		e.InputCostPerImageToken, e.OutputCostPerImageToken,
		e.InputCostPerImage, e.OutputCostPerImage,
		e.InputCostPerPixel, e.OutputCostPerPixel,
	} {
		if !validOptionalCost(value) {
			return nil, false
		}
	}
	mode := ""
	if e.Mode != nil {
		mode = strings.ToLower(strings.TrimSpace(*e.Mode))
	}
	if mode != "image_generation" && mode != "image_edit" {
		return nil, false
	}
	if !validCost(e.InputCostPerImageToken) && !validCost(e.OutputCostPerImageToken) && !validCost(e.InputCostPerImage) && !validCost(e.OutputCostPerImage) && !validCost(e.InputCostPerPixel) && !validCost(e.OutputCostPerPixel) && !validCost(e.InputCostPerToken) && !validCost(e.OutputCostPerToken) {
		return nil, false
	}
	pe := &domain.PriceEntry{Model: model, Mode: domain.PriceModeImage, Source: domain.PricingSourceLitellm, Raw: raw, Provider: e.Provider}
	// A number of multimodal models publish both text-token and image-token
	// rates under image_generation. Keep every representable component in the
	// single row so chat-compatible callers and image callers both bill from the
	// same official record.
	if v := pricePerMillion(e.InputCostPerToken); v != nil {
		pe.InputPerM = v
	}
	if v := pricePerMillion(e.OutputCostPerToken); v != nil {
		pe.OutputPerM = v
	}
	if v := cacheCost(e.CacheReadInputTokenCost); v != nil {
		pe.CacheReadPerM = v
	}
	if v := cacheCost(e.CacheCreationInputTokenCost); v != nil {
		pe.CacheWritePerM = v
	}
	if validCost(e.InputCostPerImageToken) {
		if v, ok := toMilliCentsPerMillion(*e.InputCostPerImageToken); ok {
			pe.ImgInTokPerM = &v
		}
	}
	if validCost(e.OutputCostPerImageToken) {
		if v, ok := toMilliCentsPerMillion(*e.OutputCostPerImageToken); ok {
			pe.ImgOutTokPerM = &v
		}
	}
	if v := imagePricePerImage(model, e); v != nil {
		pe.PricePerImage = v
	}
	if t := windowTokens(e.MaxInputTokens); t > 0 {
		pe.MaxInputTokens = &t
	}
	if t := windowTokens(e.MaxOutputTokens); t > 0 {
		pe.MaxOutputTokens = &t
	}
	pe.SupportsPromptCaching = e.SupportsPromptCaching
	if pe.InputPerM == nil && pe.OutputPerM == nil && pe.ImgInTokPerM == nil && pe.ImgOutTokPerM == nil && pe.PricePerImage == nil {
		return nil, false
	}
	return pe, true
}

// imagePricePerImage converts LiteLLM's flat image and pixel prices to the one
// per-image field available in PriceEntry. Pixel rows encode dimensions in the
// model id (for example 1024-x-1024/dall-e-2); when both input and output
// prices exist, a positive output price is preferred, then a positive input
// price. Explicit zero is retained only when no positive representation exists.
func imagePricePerImage(model string, e litellmEntry) *int64 {
	var zero *int64
	for _, candidate := range []*float64{e.OutputCostPerImage, e.InputCostPerImage} {
		if !validCost(candidate) {
			continue
		}
		v, ok := toMilliCentsPerImage(*candidate)
		if !ok {
			continue
		}
		if v > 0 {
			return &v
		}
		zero = &v
	}
	area, ok := imageArea(model)
	if ok {
		for _, candidate := range []*float64{e.OutputCostPerPixel, e.InputCostPerPixel} {
			if !validCost(candidate) {
				continue
			}
			price := *candidate * area
			v, converted := toMilliCentsPerImage(price)
			if !converted {
				continue
			}
			if v > 0 {
				return &v
			}
			if zero == nil {
				zero = &v
			}
		}
	}
	return zero
}

func imageArea(model string) (float64, bool) {
	for _, part := range strings.Split(model, "/") {
		pieces := strings.SplitN(strings.ToLower(part), "-x-", 2)
		if len(pieces) != 2 {
			continue
		}
		width, errW := strconv.ParseUint(pieces[0], 10, 32)
		height, errH := strconv.ParseUint(pieces[1], 10, 32)
		if errW != nil || errH != nil || width == 0 || height == 0 {
			continue
		}
		return float64(width) * float64(height), true
	}
	return 0, false
}

func parseFunctionPriceEntry(model string, raw json.RawMessage) (*domain.PriceEntry, bool) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false
	}
	if !validOptionalCost(e.InputCostPerQuery) {
		return nil, false
	}
	mode := ""
	if e.Mode != nil {
		mode = strings.ToLower(strings.TrimSpace(*e.Mode))
	}
	if mode != "search" && mode != "rerank" && mode != "vector_store" {
		return nil, false
	}
	queryPrice := e.InputCostPerQuery
	if !validCost(queryPrice) {
		// Tiered function prices are keyed by max_results_range rather than
		// prompt-token context. The current persisted price row has one base
		// per-call value, so use the lowest-range representable tier as the
		// catalogue/default price. Sort by range first because JSON array order
		// is provider data, not a stable precedence contract.
		for _, tier := range orderedFunctionTiers(parseTieredPricing(e.TieredPricing)) {
			if !validFunctionRange(tier.MaxResultsRange) {
				continue
			}
			if validCost(tier.InputCostPerQuery) {
				if _, ok := toMilliCentsPerCall(*tier.InputCostPerQuery); ok {
					queryPrice = tier.InputCostPerQuery
					break
				}
			}
		}
	}
	if !validCost(queryPrice) {
		return nil, false
	}
	v, ok := toMilliCentsPerCall(*queryPrice)
	if !ok {
		return nil, false
	}
	return &domain.PriceEntry{Model: model, Mode: domain.PriceModeCall, PricePerCall: &v, Provider: e.Provider, Source: domain.PricingSourceLitellm, Raw: raw}, true
}

func isFunctionPriceMode(mode *string) bool {
	if mode == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(*mode)) {
	case "search", "rerank", "vector_store":
		return true
	default:
		return false
	}
}

func hasTieredQueryPrice(tiers []tieredPrice) bool {
	for _, tier := range tiers {
		if tier.InputCostPerQuery != nil {
			return true
		}
	}
	return false
}

func validFunctionRange(r []float64) bool {
	if len(r) == 0 {
		return true
	}
	if len(r) > 2 {
		return false
	}
	for _, v := range r {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || math.Round(v) != v || v >= float64(uint64(1)<<63) {
			return false
		}
	}
	return len(r) != 2 || r[1] >= r[0]
}

func functionRangeLower(r []float64) float64 {
	if len(r) == 0 {
		return 0
	}
	return r[0]
}

func orderedFunctionTiers(tiers []tieredPrice) []tieredPrice {
	ordered := append([]tieredPrice(nil), tiers...)
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, rj := ordered[i].MaxResultsRange, ordered[j].MaxResultsRange
		vi, vj := validFunctionRange(ri), validFunctionRange(rj)
		if vi != vj {
			return vi
		}
		li, lj := functionRangeLower(ri), functionRangeLower(rj)
		if li != lj {
			return li < lj
		}
		// An explicitly bounded range is more specific than an unbounded
		// fallback at the same lower bound.
		if (len(ri) == 2) != (len(rj) == 2) {
			return len(ri) == 2
		}
		return len(ri) < len(rj)
	})
	return ordered
}

func cacheCost(v *float64) *int64 {
	if !validCost(v) {
		return nil
	}
	m, ok := toMilliCentsPerMillion(*v)
	if !ok {
		return nil
	}
	return &m
}

func validCost(v *float64) bool {
	// A literal 0 is an explicit free price. Positive values that round to
	// zero in the persisted unit are rejected by scaledPositiveInt64, because
	// they are not actually free and would silently undercharge.
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0
}

func validOptionalCost(v *float64) bool {
	return v == nil || validCost(v)
}

func representablePerMillion(v *float64) bool {
	if v == nil {
		return true
	}
	_, ok := toMilliCentsPerMillion(*v)
	return ok
}

func toMilliCentsPerMillion(perTokenUSD float64) (int64, bool) {
	return scaledPositiveInt64(perTokenUSD, 1e11)
}

func toMilliCentsPerImage(perImageUSD float64) (int64, bool) {
	return scaledPositiveInt64(perImageUSD, 1e5)
}

func toMilliCentsPerCall(perQueryUSD float64) (int64, bool) {
	return scaledPositiveInt64(perQueryUSD, 1e5)
}

func scaledPositiveInt64(value, scale float64) (int64, bool) {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || scale <= 0 {
		return 0, false
	}
	if value == 0 {
		return 0, true
	}
	rounded := math.Round(value * scale)
	limit := float64(uint64(1) << 63)
	if rounded <= 0 || math.IsNaN(rounded) || math.IsInf(rounded, 0) || rounded >= limit {
		return 0, false
	}
	return int64(rounded), true
}

func windowTokens(v *float64) int64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
		return 0
	}
	if *v >= float64(uint64(1)<<63) {
		return 0
	}
	rounded := math.Round(*v)
	if rounded <= 0 || rounded >= float64(uint64(1)<<63) {
		return 0
	}
	return int64(rounded)
}
