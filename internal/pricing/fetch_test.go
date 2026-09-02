// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestParsePriceEntryBasic(t *testing.T) {
	jsonStr := `{
  "gpt-4o": {
    "input_cost_per_token": 2.5e-06,
    "output_cost_per_token": 1e-05,
    "mode": "chat",
    "litellm_provider": "openai"
  },
  "gpt-image-1": {
    "input_cost_per_image_token": 1e-05,
    "output_cost_per_image": 0.02,
    "mode": "image_generation"
  },
  "search-m": {
    "input_cost_per_query": 0.0001,
    "mode": "search"
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-4o", "gpt-image-1", "search-m"}, res.Models,
		"source model keys include every JSON row, regardless of parsed pricing mode")
	require.Equal(t, 3, len(res.PriceEntries))
	require.Equal(t, 0, res.Skipped)
	byModel := map[string]*domain.PriceEntry{}
	for _, e := range res.PriceEntries {
		byModel[e.Model] = e
	}
	require.Equal(t, domain.PriceModeToken, byModel["gpt-4o"].Mode)
	require.NotNil(t, byModel["gpt-4o"].InputPerM)
	require.Equal(t, domain.PriceModeImage, byModel["gpt-image-1"].Mode)
	require.Equal(t, domain.PriceModeCall, byModel["search-m"].Mode)
}

func TestParseAcceptsAllTokenPricedLiteLLMModes(t *testing.T) {
	jsonStr := `{
  "completion-model": {
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 2e-6,
    "mode": "completion"
  },
  "realtime-model": {
    "input_cost_per_token": 3e-6,
    "output_cost_per_token": 4e-6,
    "mode": "realtime"
  },
  "legacy-model": {
    "input_cost_per_token": 5e-7,
    "output_cost_per_token": 6e-7
  },
  "embedding-model": {
    "input_cost_per_token": 7e-8,
    "output_cost_per_token": 0,
    "mode": "embedding"
  },
  "unknown-mode": {
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 2e-6,
    "mode": "one of: chat, embedding"
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"completion-model", "embedding-model", "legacy-model", "realtime-model", "unknown-mode"}, res.Models)
	require.Len(t, res.PriceEntries, 4)
	require.Equal(t, 1, res.Skipped)
	for _, entry := range res.PriceEntries {
		require.Equal(t, domain.PriceModeToken, entry.Mode)
		require.NotNil(t, entry.InputPerM)
		require.NotNil(t, entry.OutputPerM)
	}
}

func TestParseAcceptsProviderTokenRowsWithoutMode(t *testing.T) {
	res, err := Parse([]byte(`{
  "fireworks-ai-4.1b-to-16b": {
    "input_cost_per_token": 2e-7,
    "output_cost_per_token": 2e-7,
    "litellm_provider": "fireworks_ai"
  },
  "fireworks-ai-default": {
    "input_cost_per_token": 0,
    "output_cost_per_token": 0,
    "litellm_provider": "fireworks_ai"
  }
}`), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"fireworks-ai-4.1b-to-16b", "fireworks-ai-default"}, res.Models)
	require.Len(t, res.PriceEntries, 2)
	require.Zero(t, res.Skipped)
}

func TestParseAcceptsTokenPricedOCRModel(t *testing.T) {
	res, err := Parse([]byte(`{
  "vertex_ai/deepseek-ai/deepseek-ocr-maas": {
    "mode": "ocr",
    "input_cost_per_token": 3e-7,
    "output_cost_per_token": 1.2e-6,
    "ocr_cost_per_page": 0.0003
  }
}`), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 1)
	require.Zero(t, res.Skipped)
	require.Equal(t, domain.PriceModeToken, res.PriceEntries[0].Mode)
	require.Equal(t, int64(30000), *res.PriceEntries[0].InputPerM)
	require.Equal(t, int64(120000), *res.PriceEntries[0].OutputPerM)
}

func TestParseRetainsSourceModelsWhenRowsHaveNoUsablePrice(t *testing.T) {
	res, err := Parse([]byte(`{
  "priced": {"mode": "chat", "input_cost_per_token": 1e-6, "output_cost_per_token": 2e-6},
  "metadata-only": {"mode": "chat"},
  "malformed": 42
}`), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"malformed", "metadata-only", "priced"}, res.Models)
	require.Len(t, res.PriceEntries, 1)
	require.Equal(t, 2, res.Skipped)
	models, authoritative := SnapshotPriceModels(res)
	require.True(t, authoritative)
	require.Equal(t, []string{"priced"}, models,
		"reconciliation uses billable rows so an old malformed price cannot survive")
}

func TestParseNormalizesModelWhitespaceAndRejectsCollisions(t *testing.T) {
	res, err := Parse([]byte(`{
  "  spaced-model  ": {"mode":"chat","input_cost_per_token":0.000001}
}`), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"spaced-model"}, res.Models)
	require.Equal(t, "spaced-model", res.PriceEntries[0].Model)

	_, err = Parse([]byte(`{
  "duplicate": {"mode":"chat","input_cost_per_token":0.000001},
  " duplicate ": {"mode":"chat","input_cost_per_token":0.000002}
}`), nil)
	require.ErrorContains(t, err, "duplicate model after trimming")
}

func TestSnapshotPriceModelsLegacyCompleteness(t *testing.T) {
	models, authoritative := SnapshotPriceModels(&FetchResult{
		PriceEntries: []*domain.PriceEntry{{Model: "priced"}},
	})
	require.True(t, authoritative)
	require.Equal(t, []string{"priced"}, models)

	models, authoritative = SnapshotPriceModels(&FetchResult{
		PriceEntries: []*domain.PriceEntry{{Model: "priced"}},
		Skipped:      1,
	})
	require.False(t, authoritative)
	require.Nil(t, models)
}

func TestParseEmptySourceHasExplicitEmptyModels(t *testing.T) {
	res, err := Parse([]byte(`{}`), nil)
	require.NoError(t, err)
	require.NotNil(t, res.Models, "an empty source is distinct from a legacy FetchResult without Models")
	require.Empty(t, res.Models)
	require.Empty(t, res.PriceEntries)
	require.Zero(t, res.Skipped)
}

func TestParseFunctionPriceWinsOverZeroTokenPlaceholders(t *testing.T) {
	jsonStr := `{
  "amazon.rerank-v1:0": {
    "mode": "rerank",
    "input_cost_per_token": 0,
    "output_cost_per_token": 0,
    "input_cost_per_query": 0.001
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 1)
	require.Zero(t, res.Skipped)
	entry := res.PriceEntries[0]
	require.Equal(t, domain.PriceModeCall, entry.Mode)
	require.Nil(t, entry.InputPerM, "zero token placeholders must not become a token price")
	require.Nil(t, entry.OutputPerM, "zero token placeholders must not become a token price")
	require.NotNil(t, entry.PricePerCall)
	require.Equal(t, int64(100), *entry.PricePerCall)
}

func TestParseFunctionTieredPricingUsesLowestMaxResultsRange(t *testing.T) {
	jsonStr := `{
  "exa_ai/search": {
    "mode": "search",
    "tiered_pricing": [
      {"input_cost_per_query": 0.025, "max_results_range": [26, 100]},
      {"input_cost_per_query": 0.005, "max_results_range": [0, 25]}
    ]
  },
  "firecrawl/search": {
    "mode": "search",
    "tiered_pricing": [
      {"input_cost_per_query": 0.00332, "max_results_range": [11, 20]},
      {"input_cost_per_query": 0.00166, "max_results_range": [1, 10]}
    ]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 2)
	byModel := make(map[string]*domain.PriceEntry, len(res.PriceEntries))
	for _, entry := range res.PriceEntries {
		byModel[entry.Model] = entry
	}
	require.Equal(t, int64(500), *byModel["exa_ai/search"].PricePerCall)
	require.Equal(t, int64(166), *byModel["firecrawl/search"].PricePerCall)
}

func TestParseDoesNotRescueMalformedFunctionPriceAsToken(t *testing.T) {
	jsonStr := `{
  "bad-rerank": {
    "mode": "rerank",
    "input_cost_per_token": 0,
    "output_cost_per_token": 0,
    "input_cost_per_query": -0.001
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Empty(t, res.PriceEntries)
	require.Equal(t, 1, res.Skipped)
}

func TestParseDoesNotRescueUnrepresentableTieredFunctionPriceAsFreeToken(t *testing.T) {
	jsonStr := `{
  "tiny-tiered-rerank": {
    "mode": "rerank",
    "input_cost_per_token": 0,
    "output_cost_per_token": 0,
    "tiered_pricing": [{"input_cost_per_query": 1e-20, "max_results_range": [0, 10]}]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Empty(t, res.PriceEntries)
	require.Equal(t, 1, res.Skipped)
}

func TestParseTieredPricingPromotesBaseAndKeepsContextRanges(t *testing.T) {
	jsonStr := `{
  "dashscope/qwen-flash": {
    "mode": "chat",
    "tiered_pricing": [
      {"input_cost_per_token": 2.5e-7, "output_cost_per_token": 2e-6, "range": [256000, 1000000]},
      {"input_cost_per_token": 5e-8, "output_cost_per_token": 4e-7, "range": [0, 256000]}
    ]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 1)
	entry := res.PriceEntries[0]
	require.Equal(t, "dashscope/qwen-flash", entry.Model)
	require.Equal(t, int64(5000), *entry.InputPerM)
	require.Equal(t, int64(40000), *entry.OutputPerM)
	require.Len(t, res.Variants, 1)
	variant := res.Variants[0]
	require.Equal(t, int64(256000), *variant.CtxMin)
	require.Equal(t, int64(1000000), *variant.CtxMax)
	require.Equal(t, int64(25000), *variant.SetInputPerM)
	require.Equal(t, int64(200000), *variant.SetOutputPerM)
}

func TestParseTieredPricingDoesNotPromoteHighContextTier(t *testing.T) {
	jsonStr := `{
  "high-context-only": {
    "mode": "chat",
    "tiered_pricing": [
      {"input_cost_per_token": 2e-6, "output_cost_per_token": 4e-6, "range": [32000, 128000]}
    ]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	// The source key remains visible for completeness diagnostics, but there is
	// no safe unconditional price to persist for requests below 32k tokens.
	require.Equal(t, []string{"high-context-only"}, res.Models)
	require.Empty(t, res.PriceEntries)
	require.Empty(t, res.Variants)
	require.Equal(t, 1, res.Skipped)
}

func TestParseTieredPricingFillsOnlyMissingTopLevelComponent(t *testing.T) {
	jsonStr := `{
  "partial-tiered": {
    "mode": "responses",
    "input_cost_per_token": 1e-6,
    "tiered_pricing": [{"input_cost_per_token": 1e-6, "output_cost_per_token": 2e-6, "range": [0, 100000]}]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 1)
	require.Equal(t, int64(100000), *res.PriceEntries[0].InputPerM)
	require.Equal(t, int64(200000), *res.PriceEntries[0].OutputPerM)
}

func TestParseAboveVariantsOrdersMostSpecificThresholdFirst(t *testing.T) {
	jsonStr := `{
  "context-model": {
    "mode": "chat",
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 2e-6,
    "input_cost_per_token_above_100k_tokens": 3e-6,
    "output_cost_per_token_above_100k_tokens": 6e-6,
    "input_cost_per_token_above_200k_tokens": 5e-6,
    "output_cost_per_token_above_200k_tokens": 10e-6
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.Variants, 2)
	require.Equal(t, int64(200000), *res.Variants[0].CtxMin)
	require.Equal(t, int64(100000), *res.Variants[1].CtxMin)
}

func TestParseAboveVariantsSortsCombinedConditionsBeforeFallback(t *testing.T) {
	jsonStr := `{
  "mixed-context-tiers": {
    "mode": "chat",
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 2e-6,
    "input_cost_per_token_above_200k_tokens_priority": 4e-6,
    "output_cost_per_token_above_200k_tokens_priority": 8e-6,
    "input_cost_per_token_above_300k_tokens": 3e-6,
    "output_cost_per_token_above_300k_tokens": 6e-6
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.Variants, 2)
	// ResolveEntryPrices stops at the first matching variant. A row carrying
	// both the requested service tier and a context bound must therefore beat
	// a generic context fallback, even when the fallback has a higher bound.
	require.Equal(t, int64(200000), *res.Variants[0].CtxMin)
	require.Equal(t, "priority", *res.Variants[0].ServiceTier)
	require.Equal(t, int64(300000), *res.Variants[1].CtxMin)
	require.Nil(t, res.Variants[1].ServiceTier)
	rp, ok := domain.ResolveEntryPrices(res.PriceEntries[0], res.Variants, "priority", 300000, time.Now())
	require.True(t, ok)
	require.Equal(t, int64(400000), *rp.InputPerM, "combined priority+context price must be reachable")
}

func TestParseAboveVariantsSortsContextThresholdsWithinSameSpecificity(t *testing.T) {
	jsonStr := `{
  "mixed-context-tiers": {
    "mode": "chat",
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 2e-6,
    "input_cost_per_token_above_100k_tokens_priority": 3e-6,
    "output_cost_per_token_above_100k_tokens_priority": 6e-6,
    "input_cost_per_token_above_300k_tokens_priority": 5e-6,
    "output_cost_per_token_above_300k_tokens_priority": 10e-6
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.Variants, 2)
	require.Equal(t, int64(300000), *res.Variants[0].CtxMin)
	require.Equal(t, int64(100000), *res.Variants[1].CtxMin)
}

func TestParseRetainsOneSidedTokenPrice(t *testing.T) {
	jsonStr := `{
  "output-only": {"mode": "chat", "output_cost_per_token": 3e-6},
  "input-only": {"mode": "embedding", "input_cost_per_token": 4e-7}
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 2)
	byModel := make(map[string]*domain.PriceEntry, len(res.PriceEntries))
	for _, entry := range res.PriceEntries {
		byModel[entry.Model] = entry
	}
	require.Nil(t, byModel["output-only"].InputPerM)
	require.Equal(t, int64(300000), *byModel["output-only"].OutputPerM)
	require.Equal(t, int64(40000), *byModel["input-only"].InputPerM)
	require.Nil(t, byModel["input-only"].OutputPerM)
}

func TestParseImagePixelPriceUsesDeclaredDimensions(t *testing.T) {
	jsonStr := `{
  "1024-x-1024/dall-e-2": {
    "mode": "image_generation",
    "input_cost_per_pixel": 1e-8,
    "output_cost_per_pixel": 0
  },
  "dall-e-2": {
    "mode": "image_generation",
    "input_cost_per_pixel": 1e-8
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 1)
	require.Equal(t, "1024-x-1024/dall-e-2", res.PriceEntries[0].Model)
	require.NotNil(t, res.PriceEntries[0].PricePerImage)
	require.Equal(t, int64(1049), *res.PriceEntries[0].PricePerImage)
	require.Equal(t, 1, res.Skipped)
}

func TestParsePriceEntryVariants(t *testing.T) {
	jsonStr := `{
  "gpt-5": {
    "input_cost_per_token": 2e-06,
    "output_cost_per_token": 8e-06,
    "input_cost_per_token_priority": 3e-06,
    "output_cost_per_token_priority": 1.2e-05,
    "mode": "chat",
    "provider_specific_entry": {"fast": 2.0}
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Equal(t, 1, len(res.PriceEntries))
	require.Equal(t, 2, len(res.Variants))
	// priority and fast variants
	foundPriority := false
	foundFast := false
	for _, v := range res.Variants {
		if v.ServiceTier != nil && *v.ServiceTier == "priority" {
			foundPriority = true
		}
		if v.ServiceTier != nil && *v.ServiceTier == "fast" {
			foundFast = true
		}
	}
	require.True(t, foundPriority)
	require.True(t, foundFast)
}

func TestParseProviderFastMultiplierNeverRoundsPositiveValueToFree(t *testing.T) {
	res, err := Parse([]byte(`{
  "tiny-fast": {
    "mode":"chat",
    "input_cost_per_token":0.000001,
    "provider_specific_entry":{"fast":0.000001}
  },
  "oversized-fast": {
    "mode":"chat",
    "input_cost_per_token":0.000001,
    "provider_specific_entry":{"fast":11}
  },
  "normal-fast": {
    "mode":"chat",
    "input_cost_per_token":0.000001,
    "provider_specific_entry":{"fast":2}
  }
}`), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 3)
	require.Len(t, res.Variants, 1)
	require.Equal(t, "normal-fast", res.Variants[0].Model)
	require.NotNil(t, res.Variants[0].MultBP)
	require.Equal(t, 20000, *res.Variants[0].MultBP)
}

func TestParseTieredPricingPromotesBaseAndAddsBoundedVariants(t *testing.T) {
	jsonStr := `{
  "qwen-tiered": {
    "mode": "chat",
    "tiered_pricing": [
      {"input_cost_per_token": 1e-6, "output_cost_per_token": 4e-6, "range": [0, 32000]},
      {"input_cost_per_token": 2e-6, "output_cost_per_token": 8e-6, "range": [32000, 128000]},
      {"input_cost_per_token": 3e-6, "output_cost_per_token": 12e-6, "range": [128000, 256000]}
    ]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 1)
	require.Equal(t, int64(100000), *res.PriceEntries[0].InputPerM)
	require.Equal(t, int64(400000), *res.PriceEntries[0].OutputPerM)
	require.Len(t, res.Variants, 2)
	require.Equal(t, int64(128000), *res.Variants[0].CtxMin)
	require.Equal(t, int64(256000), *res.Variants[0].CtxMax)
	require.Equal(t, int64(300000), *res.Variants[0].SetInputPerM)
	require.Equal(t, int64(32000), *res.Variants[1].CtxMin)
	require.Equal(t, int64(128000), *res.Variants[1].CtxMax)
	require.Equal(t, int64(200000), *res.Variants[1].SetInputPerM)
}

func TestParseTieredPricingSkipsDuplicateZeroContextTiers(t *testing.T) {
	jsonStr := `{
  "duplicate-base": {
    "mode": "chat",
    "tiered_pricing": [
      {"input_cost_per_token": 1e-6, "output_cost_per_token": 2e-6},
      {"input_cost_per_token": 3e-6, "output_cost_per_token": 4e-6, "range": [0, 1000]},
      {"input_cost_per_token": 5e-6, "output_cost_per_token": 6e-6, "range": [1000, 2000]}
    ]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 1)
	require.Len(t, res.Variants, 1)
	require.Equal(t, int64(1000), *res.Variants[0].CtxMin)
	require.Equal(t, int64(500000), *res.Variants[0].SetInputPerM)
}

func TestParseTieredPricingKeepsPriorityAndContextSpecificRowsFirst(t *testing.T) {
	jsonStr := `{
  "qwen-tiered": {
    "mode": "chat",
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 4e-6,
    "input_cost_per_token_priority": 2e-6,
    "output_cost_per_token_priority": 8e-6,
    "tiered_pricing": [
      {"input_cost_per_token": 1e-6, "output_cost_per_token": 4e-6, "range": [0, 32000]},
      {"input_cost_per_token": 3e-6, "output_cost_per_token": 12e-6, "range": [32000, 128000]}
    ]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.Variants, 2)
	require.Equal(t, "priority", *res.Variants[0].ServiceTier)
	// Sequence is deterministic even though the source JSON object is unordered.
	require.Equal(t, 1, res.Variants[0].Seq)
	require.Equal(t, 2, res.Variants[1].Seq)
	require.Equal(t, int64(32000), *res.Variants[1].CtxMin)
}

func TestParseSupportsOneSidedTokenPrices(t *testing.T) {
	jsonStr := `{
  "input-only": {"mode": "embedding", "input_cost_per_token": 7e-7},
  "output-only": {"mode": "completion", "output_cost_per_token": 9e-7}
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 2)
	for _, entry := range res.PriceEntries {
		switch entry.Model {
		case "input-only":
			require.NotNil(t, entry.InputPerM)
			require.Nil(t, entry.OutputPerM)
		case "output-only":
			require.Nil(t, entry.InputPerM)
			require.NotNil(t, entry.OutputPerM)
		}
	}
}

func TestParseReturnsModelsInStableOrder(t *testing.T) {
	jsonStr := `{
  "z-model": {"mode": "chat", "input_cost_per_token": 1e-6},
  "a-model": {"mode": "search", "input_cost_per_query": 0.01},
  "m-model": {"mode": "image_generation", "output_cost_per_image": 0.02}
}`
	for attempt := 0; attempt < 5; attempt++ {
		res, err := Parse([]byte(jsonStr), nil)
		require.NoError(t, err)
		require.Len(t, res.PriceEntries, 3)
		require.Equal(t, []string{"a-model", "m-model", "z-model"}, []string{
			res.PriceEntries[0].Model,
			res.PriceEntries[1].Model,
			res.PriceEntries[2].Model,
		})
	}
}

func TestParseImagePerImageAndPixelPricing(t *testing.T) {
	jsonStr := `{
  "1024-x-1024/dall-e-2": {
    "mode": "image_generation",
    "input_cost_per_pixel": 1e-7,
    "output_cost_per_pixel": 0
  },
  "flat-image": {
    "mode": "image_generation",
    "input_cost_per_image": 0.01,
    "output_cost_per_image": 0.02
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 2)
	byModel := make(map[string]*domain.PriceEntry, len(res.PriceEntries))
	for _, entry := range res.PriceEntries {
		byModel[entry.Model] = entry
	}
	// 1e-7 USD x 1024 x 1024 = 0.1048576 USD, rounded to 10486 units.
	require.Equal(t, int64(10486), *byModel["1024-x-1024/dall-e-2"].PricePerImage)
	require.Equal(t, int64(2000), *byModel["flat-image"].PricePerImage)
}

func TestParsePreservesCacheAndAboveContextVariants(t *testing.T) {
	jsonStr := `{
  "cache-model": {
    "mode": "chat",
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 2e-6,
    "cache_read_input_token_cost": 2e-7,
    "cache_creation_input_token_cost": 4e-7,
    "input_cost_per_token_above_200k_tokens": 3e-6,
    "output_cost_per_token_above_200k_tokens": 6e-6,
    "cache_read_input_token_cost_above_200k_tokens": 5e-7,
    "cache_creation_input_token_cost_above_200k_tokens": 7e-7,
    "input_cost_per_token_above_200k_tokens_priority": 4e-6,
    "output_cost_per_token_above_200k_tokens_priority": 8e-6
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.Variants, 2)
	// Context + service tier is more specific than the generic context row.
	require.Equal(t, "priority", *res.Variants[0].ServiceTier)
	require.Equal(t, int64(200000), *res.Variants[0].CtxMin)
	require.Equal(t, int64(400000), *res.Variants[0].SetInputPerM)
	require.Equal(t, int64(800000), *res.Variants[0].SetOutputPerM)
	require.Equal(t, int64(200000), *res.Variants[1].CtxMin)
	require.Nil(t, res.Variants[1].ServiceTier)
	require.Equal(t, int64(300000), *res.Variants[1].SetInputPerM)
	require.Equal(t, int64(50000), *res.Variants[1].SetCacheReadPerM)
}

func TestParsePreservesExplicitZeroPrices(t *testing.T) {
	jsonStr := `{
  "free-chat": {
    "input_cost_per_token": 0,
    "output_cost_per_token": 0,
    "cache_read_input_token_cost": 0,
    "cache_creation_input_token_cost": 0,
    "mode": "chat"
  },
  "free-tier": {
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 2e-6,
    "input_cost_per_token_priority": 0,
    "output_cost_per_token_priority": 0,
    "mode": "chat"
  },
  "free-image": {
    "input_cost_per_image_token": 0,
    "output_cost_per_image_token": 0,
    "output_cost_per_image": 0,
    "mode": "image_generation"
  },
  "free-search": {
    "input_cost_per_query": 0,
    "mode": "search"
  },
  "free-segment": {
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 2e-6,
    "input_cost_per_token_above_100k_tokens": 0,
    "output_cost_per_token_above_100k_tokens": 0,
    "mode": "chat"
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 5, "explicitly free entries must not be skipped")
	require.Zero(t, res.Skipped)

	byModel := make(map[string]*domain.PriceEntry, len(res.PriceEntries))
	for _, entry := range res.PriceEntries {
		byModel[entry.Model] = entry
	}

	freeChat := byModel["free-chat"]
	require.NotNil(t, freeChat)
	require.NotNil(t, freeChat.InputPerM)
	require.Zero(t, *freeChat.InputPerM)
	require.NotNil(t, freeChat.OutputPerM)
	require.Zero(t, *freeChat.OutputPerM)
	require.NotNil(t, freeChat.CacheReadPerM)
	require.Zero(t, *freeChat.CacheReadPerM)
	require.NotNil(t, freeChat.CacheWritePerM)
	require.Zero(t, *freeChat.CacheWritePerM)

	freeImage := byModel["free-image"]
	require.NotNil(t, freeImage)
	require.NotNil(t, freeImage.ImgInTokPerM)
	require.Zero(t, *freeImage.ImgInTokPerM)
	require.NotNil(t, freeImage.ImgOutTokPerM)
	require.Zero(t, *freeImage.ImgOutTokPerM)
	require.NotNil(t, freeImage.PricePerImage)
	require.Zero(t, *freeImage.PricePerImage)

	freeSearch := byModel["free-search"]
	require.NotNil(t, freeSearch)
	require.NotNil(t, freeSearch.PricePerCall)
	require.Zero(t, *freeSearch.PricePerCall)

	var priority, segment bool
	for _, variant := range res.Variants {
		switch {
		case variant.Model == "free-tier" && variant.ServiceTier != nil && *variant.ServiceTier == "priority":
			priority = true
			require.NotNil(t, variant.SetInputPerM)
			require.Zero(t, *variant.SetInputPerM)
			require.NotNil(t, variant.SetOutputPerM)
			require.Zero(t, *variant.SetOutputPerM)
		case variant.Model == "free-segment" && variant.CtxMin != nil:
			segment = true
			require.Equal(t, int64(100_000), *variant.CtxMin)
			require.NotNil(t, variant.SetInputPerM)
			require.Zero(t, *variant.SetInputPerM)
			require.NotNil(t, variant.SetOutputPerM)
			require.Zero(t, *variant.SetOutputPerM)
		}
	}
	require.True(t, priority, "explicitly free priority tier must be retained")
	require.True(t, segment, "explicitly free above_* tier must be retained")
}

func TestParseRejectsTinyPositivePricesInsteadOfTreatingThemAsFree(t *testing.T) {
	jsonStr := `{
  "tiny-chat": {
    "input_cost_per_token": 1e-20,
    "output_cost_per_token": 1e-6,
    "mode": "chat"
  },
  "tiny-image": {
    "input_cost_per_image_token": 1e-20,
    "mode": "image_generation"
  },
  "tiny-search": {
    "input_cost_per_query": 1e-20,
    "mode": "search"
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Empty(t, res.PriceEntries, "positive values that round to zero are not free prices")
	require.Equal(t, 3, res.Skipped)
}

func TestParseKeepsModelWhenOptionalTokenComponentsAreTooSmall(t *testing.T) {
	jsonStr := `{
  "tiny-optional": {
    "mode": "chat",
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 2e-6,
    "cache_read_input_token_cost": 1e-20,
    "input_cost_per_token_priority": 1e-20,
    "output_cost_per_token_priority": 3e-6,
    "tiered_pricing": [{"input_cost_per_token": 1e-20, "output_cost_per_token": 4e-6, "range": [1000, 2000]}]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 1)
	require.Zero(t, res.Skipped)
	require.Nil(t, res.PriceEntries[0].CacheReadPerM)
	require.Len(t, res.Variants, 2)
	for _, variant := range res.Variants {
		if variant.ServiceTier != nil {
			require.Equal(t, "priority", *variant.ServiceTier)
			require.Nil(t, variant.SetInputPerM)
			require.Equal(t, int64(300000), *variant.SetOutputPerM)
		} else {
			require.Equal(t, int64(1000), *variant.CtxMin)
			require.Nil(t, variant.SetInputPerM)
			require.Equal(t, int64(400000), *variant.SetOutputPerM)
		}
	}
}

func TestParseRejectsOverflowingCostsAndMalformedThresholds(t *testing.T) {
	jsonStr := `{
  "too-expensive": {"input_cost_per_token": 1e308, "output_cost_per_token": 1e-5, "mode": "chat"},
  "bad-threshold": {"input_cost_per_token": 1e-6, "output_cost_per_token": 1e-5, "input_cost_per_token_above_12junk_tokens": 1e-5, "output_cost_per_token_above_12junk_tokens": 1e-5, "mode": "chat"},
  "huge-threshold": {"input_cost_per_token": 1e-6, "output_cost_per_token": 1e-5, "input_cost_per_token_above_9223372036854777k_tokens": 1e-5, "output_cost_per_token_above_9223372036854777k_tokens": 1e-5, "mode": "chat"}
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Len(t, res.PriceEntries, 2)
	for _, entry := range res.PriceEntries {
		require.NotEqual(t, "too-expensive", entry.Model)
	}
	require.Empty(t, res.Variants, "malformed and overflowing above_* keys must be ignored")
}

func TestParseRejectsMalformedTieredTokenPricing(t *testing.T) {
	jsonStr := `{
  "reversed-range": {
    "mode": "chat",
    "tiered_pricing": [{"input_cost_per_token": 1e-6, "range": [1000, 10]}]
  },
  "fractional-range": {
    "mode": "chat",
    "tiered_pricing": [{"input_cost_per_token": 1e-6, "range": [0.5, 1000]}]
  },
  "negative-tier-price": {
    "mode": "chat",
    "tiered_pricing": [{"input_cost_per_token": -1e-6, "range": [0, 1000]}]
  },
  "tiny-tier-price": {
    "mode": "chat",
    "tiered_pricing": [{"input_cost_per_token": 1e-20, "range": [0, 1000]}]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Empty(t, res.PriceEntries)
	require.Equal(t, 4, res.Skipped)
}

func TestParseRejectsMalformedOptionalImageAndCallPrices(t *testing.T) {
	jsonStr := `{
  "bad-image": {
    "mode": "image_generation",
    "input_cost_per_image_token": -1e-6,
    "output_cost_per_image": 0.02
  },
  "bad-search": {
    "mode": "search",
    "input_cost_per_query": -0.01,
    "tiered_pricing": [{"input_cost_per_query": 0.02}]
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Empty(t, res.PriceEntries)
	require.Equal(t, 2, res.Skipped)
}

func TestScaledPositiveInt64RejectsNonFiniteAndOverflow(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), 1e308} {
		_, ok := toMilliCentsPerMillion(value)
		require.False(t, ok)
	}
	_, ok := toMilliCentsPerMillion(1e-20)
	require.False(t, ok, "a price that rounds to zero is not a usable price")
}
