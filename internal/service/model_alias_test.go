// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestModelLookupKeyExactMatchIsCaseSensitive(t *testing.T) {
	keys := []string{"gpt-4o", "gpt-4o-2024-08-06", "Foo"}

	got, ok := modelLookupKey(keys, "gpt-4o-2024-08-06")
	require.True(t, ok)
	require.Equal(t, "gpt-4o-2024-08-06", got, "exact identifiers must win over aliases")

	got, ok = modelLookupKey(keys, "foo")
	require.False(t, ok, "case-only duplicates must not be guessed")
	require.Empty(t, got)
}

func TestModelLookupKeyReleaseAndProviderAliases(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keys    []string
		request string
		want    string
		ok      bool
	}{
		{
			name:    "dated snapshot falls back to canonical root",
			keys:    []string{"deepseek-v4-pro"},
			request: "deepseek-v4-pro-0813",
			want:    "deepseek-v4-pro",
			ok:      true,
		},
		{
			name:    "latest alias falls back to canonical root",
			keys:    []string{"gpt-4o"},
			request: "gpt-4o-latest",
			want:    "gpt-4o",
			ok:      true,
		},
		{
			name:    "stable alias falls back to canonical root",
			keys:    []string{"gpt-4o"},
			request: "gpt-4o.stable",
			want:    "gpt-4o",
			ok:      true,
		},
		{
			name:    "provider request accepts unique unqualified root",
			keys:    []string{"gpt-4o"},
			request: "openai/gpt-4o",
			want:    "gpt-4o",
			ok:      true,
		},
		{
			name:    "unqualified request accepts one provider row",
			keys:    []string{"together_ai/zai-org/GLM-5.3"},
			request: "glm-5.3",
			want:    "together_ai/zai-org/GLM-5.3",
			ok:      true,
		},
		{
			name:    "dated provider row accepts basename case variation",
			keys:    []string{"together_ai/zai-org/GLM-5.3-0813"},
			request: "glm-5.3-0813",
			want:    "together_ai/zai-org/GLM-5.3-0813",
			ok:      true,
		},
		{
			name:    "provider snapshot request accepts unique root",
			keys:    []string{"deepseek-v4-pro"},
			request: "relay/deepseek-v4-pro-0813",
			want:    "deepseek-v4-pro",
			ok:      true,
		},
		{
			name:    "different providers remain ambiguous",
			keys:    []string{"provider-a/deepseek-v4-pro-0813", "provider-b/deepseek-v4-pro-0813"},
			request: "deepseek-v4-pro-0813",
			ok:      false,
		},
		{
			name:    "provider-qualified candidates remain ambiguous",
			keys:    []string{"provider-a/gpt-4o", "provider-b/gpt-4o"},
			request: "relay/gpt-4o",
			ok:      false,
		},
		{
			name:    "different qualified provider is rejected even when unique",
			keys:    []string{"provider-a/gpt-4o"},
			request: "relay/gpt-4o",
			ok:      false,
		},
		{
			name:    "same qualified provider keeps snapshot alias",
			keys:    []string{"relay/gpt-4o-2024-08-06"},
			request: "relay/gpt-4o",
			want:    "relay/gpt-4o-2024-08-06",
			ok:      true,
		},
		{
			name:    "provider dated row accepts numeric separator alias",
			keys:    []string{"volcengine/doubao-seed-2-0-pro-260215"},
			request: "doubao-seed-2.0-pro",
			want:    "volcengine/doubao-seed-2-0-pro-260215",
			ok:      true,
		},
		{
			name:    "explicit dated numeric alias accepts provider row",
			keys:    []string{"volcengine/doubao-seed-2-0-pro-260215"},
			request: "doubao-seed-2.0-pro-260215",
			want:    "volcengine/doubao-seed-2-0-pro-260215",
			ok:      true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := modelLookupKey(tc.keys, tc.request)
			require.Equal(t, tc.ok, ok)
			if tc.ok {
				require.Equal(t, tc.want, got)
			} else {
				require.Empty(t, got)
			}
		})
	}
}

func TestModelLookupKeyDoesNotCollapseVersionsOrPunctuation(t *testing.T) {
	for _, model := range []string{
		"claude-opus-4-6",
		"claude-opus-4-7",
		"foo.bar",
		"foo-bar",
		"foo-2024-13-40",
		"foo-20240230",
	} {
		base, hasSnapshot := stripModelSnapshot(model)
		require.False(t, hasSnapshot, "model %q must not be treated as a release snapshot", model)
		require.Equal(t, model, base)
	}

	got, ok := modelLookupKey([]string{"foo.bar"}, "foo-bar")
	require.False(t, ok, "punctuation is part of a provider model identifier")
	require.Empty(t, got)
}

func TestStripModelSnapshotRecognizesSupportedDateForms(t *testing.T) {
	for _, tc := range []struct {
		model string
		base  string
	}{
		{model: "gpt-4o-2024-08-06", base: "gpt-4o"},
		{model: "deepseek-v4-pro-20240813", base: "deepseek-v4-pro"},
		{model: "deepseek-v4-pro-0813", base: "deepseek-v4-pro"},
		{model: "gemini-2.5-pro-preview-05-06", base: "gemini-2.5-pro-preview"},
		{model: "gpt-4o-latest", base: "gpt-4o"},
		{model: "gpt-4o_stable", base: "gpt-4o"},
		{model: "doubao-seed-2-0-pro-260215", base: "doubao-seed-2-0-pro"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			got, ok := stripModelSnapshot(tc.model)
			require.True(t, ok)
			require.Equal(t, tc.base, got)
		})
	}
}

func TestNumericModelAliasIsConservative(t *testing.T) {
	for _, tc := range []struct {
		name  string
		left  string
		right string
		want  bool
	}{
		{name: "version dot and dash", left: "doubao-seed-2.0-pro", right: "doubao-seed-2-0-pro", want: true},
		{name: "claude release number remains distinct", left: "claude-opus-4-6", right: "claude-opus-4-7", want: false},
		{name: "large dimension remains distinct", left: "qwen3-235b", right: "qwen3.235b", want: false},
		{name: "text punctuation remains distinct", left: "foo.bar", right: "foo-bar", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, numericModelAlias(tc.left) == numericModelAlias(tc.right))
		})
	}
}

func TestPricingProjectionAndBatchLookupUseAliasIndex(t *testing.T) {
	fs := newFakeStore()
	inPrice, outPrice := int64(250000), int64(1000000)
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{{
		Model: "deepseek-v4-pro", Mode: domain.PriceModeToken,
		InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceLitellm,
	}})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)

	catalogue, resolved, err := svc.pricingProjectionForModels(context.Background(), []string{"deepseek-v4-pro-0813"}, "", 0, time.Now())
	require.NoError(t, err)
	require.Contains(t, catalogue, "deepseek-v4-pro-0813")
	require.Equal(t, inPrice, *catalogue["deepseek-v4-pro-0813"].InputPerM)
	require.Contains(t, resolved, "deepseek-v4-pro-0813")
	require.Equal(t, outPrice, *resolved["deepseek-v4-pro-0813"].OutputPerM)

	entries, err := svc.PriceEntriesForModels(context.Background(), []string{"deepseek-v4-pro-0813"})
	require.NoError(t, err)
	require.Contains(t, entries, "deepseek-v4-pro-0813")
	require.Equal(t, inPrice, *entries["deepseek-v4-pro-0813"].InputPerM)

	resolvedBatch, err := svc.ResolvedPricesForModels(context.Background(), []string{"deepseek-v4-pro-0813"}, "", 0, time.Now())
	require.NoError(t, err)
	require.Contains(t, resolvedBatch, "deepseek-v4-pro-0813")
	require.Equal(t, outPrice, *resolvedBatch["deepseek-v4-pro-0813"].OutputPerM)
}

func TestPricingAliasCoalescesIdenticalProviderPrices(t *testing.T) {
	fs := newFakeStore()
	for _, model := range []string{"provider-a/deepseek-v4-pro-0813", "provider-b/deepseek-v4-pro-0813"} {
		inPrice, outPrice := int64(250000), int64(1000000)
		_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
			{Model: model, Mode: domain.PriceModeToken, InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceLitellm},
		})
		require.NoError(t, err)
	}
	svc := newPricingSvc(t, fs)
	catalogue, resolved, err := svc.pricingProjectionForModels(context.Background(), []string{"deepseek-v4-pro-0813"}, "", 0, time.Now())
	require.NoError(t, err)
	require.Contains(t, catalogue, "deepseek-v4-pro-0813")
	require.Contains(t, resolved, "deepseek-v4-pro-0813")
	require.Equal(t, int64(250000), *catalogue["deepseek-v4-pro-0813"].InputPerM)
	require.Equal(t, int64(1000000), *resolved["deepseek-v4-pro-0813"].OutputPerM)
}

func TestPricingAliasWithDifferentProviderPricesStaysUnpriced(t *testing.T) {
	fs := newFakeStore()
	for i, model := range []string{"provider-a/deepseek-v4-pro-0813", "provider-b/deepseek-v4-pro-0813"} {
		inPrice, outPrice := int64(250000+i*10000), int64(1000000+i*10000)
		_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
			{Model: model, Mode: domain.PriceModeToken, InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceLitellm},
		})
		require.NoError(t, err)
	}
	svc := newPricingSvc(t, fs)
	catalogue, resolved, err := svc.pricingProjectionForModels(context.Background(), []string{"deepseek-v4-pro-0813"}, "", 0, time.Now())
	require.NoError(t, err)
	require.NotContains(t, catalogue, "deepseek-v4-pro-0813")
	require.NotContains(t, resolved, "deepseek-v4-pro-0813")
}

func TestPricingAliasMatchesVolcengineNumericReleaseAndDate(t *testing.T) {
	fs := newFakeStore()
	inPrice, outPrice := int64(46000), int64(230000)
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{{
		Model: "volcengine/doubao-seed-2-0-pro-260215", Mode: domain.PriceModeToken,
		InputPerM: &inPrice, OutputPerM: &outPrice, Source: domain.PricingSourceLitellm,
	}})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	for _, requested := range []string{"doubao-seed-2.0-pro", "doubao-seed-2.0-pro-260215", "volcengine/doubao-seed-2.0-pro"} {
		got, ok := svc.ResolvePrices(requested, 0, "", time.Now())
		require.Truef(t, ok, "expected alias %q to resolve", requested)
		require.Equal(t, inPrice, *got.InputPerM)
		require.Equal(t, outPrice, *got.OutputPerM)
	}
}
