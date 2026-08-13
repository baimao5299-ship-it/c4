// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// litellmFixtureJSON 模拟 litellm 官方价格表结构（model_name → 行）：
// 正常行（含 max_tokens/cache 价/元数据/未映射字段）+ 无效行（缺 output 价 /
// 0 价 / null / 负价 / 字符串类型 / 溢出数字 / 非对象行）。
const litellmFixtureJSON = `{
  "gpt-4o": {
    "input_cost_per_token": 2.5e-06,
    "output_cost_per_token": 1e-05,
    "cache_read_input_token_cost": 1e-06,
    "cache_creation_input_token_cost": 2e-06,
    "max_input_tokens": 128000,
    "max_output_tokens": 16384,
    "litellm_provider": "openai",
    "mode": "chat",
    "supports_prompt_caching": true,
    "input_cost_per_character": 1.0,
    "output_cost_per_character": 2.0,
    "rpm": 600,
    "supports_vision": true
  },
  "claude-3-5-sonnet": {
    "input_cost_per_token": 3e-06,
    "output_cost_per_token": 1.5e-05,
    "max_input_tokens": 200000,
    "litellm_provider": "anthropic",
    "mode": "chat"
  },
  "tiny-rounding": {
    "input_cost_per_token": 6.123456789e-07,
    "output_cost_per_token": 1e-06,
    "mode": "chat"
  },
  "no-max-tokens": {
    "input_cost_per_token": 1e-06,
    "output_cost_per_token": 2e-06,
    "max_input_tokens": null,
    "max_output_tokens": 0,
    "cache_read_input_token_cost": 0,
    "cache_creation_input_token_cost": null,
    "mode": "chat"
  },
  "missing-output": {
    "input_cost_per_token": 1e-06
  },
  "zero-cost": {
    "input_cost_per_token": 0,
    "output_cost_per_token": 0
  },
  "null-cost": {
    "input_cost_per_token": null,
    "output_cost_per_token": 1e-06
  },
  "negative-cost": {
    "input_cost_per_token": -1e-06,
    "output_cost_per_token": 1e-06
  },
  "string-cost": {
    "input_cost_per_token": "0.000002",
    "output_cost_per_token": 1e-06
  },
  "overflow-cost": {
    "input_cost_per_token": 1e999,
    "output_cost_per_token": 1e-06
  },
  "not-an-object": 42
}`

// TestParseValidRows 解析 + 毫分换算精确断言（×1e11 四舍五入）：
// 2.5e-6 USD/token → 250000 毫分/1M（=$2.5/1M）；3e-6 → 300000；1.5e-5 →
// 1500000；6.123456789e-7 → 61235（round）；cache 价换算 + 元数据提取 +
// max_tokens roundtrip（含 null/0 → nil）。
func TestParseValidRows(t *testing.T) {
	res, err := Parse([]byte(litellmFixtureJSON), nil)
	require.NoError(t, err)
	require.Equal(t, 4, len(res.Rows), "4 个数值有效行")
	require.Equal(t, 7, res.Skipped, "7 个无效行跳过")

	byModel := map[string]*domain.Pricing{}
	for _, p := range res.Rows {
		byModel[p.Model] = p
	}

	// gpt-4o：换算精确 + max_tokens roundtrip + cache 价换算 + 元数据提取 +
	// raw 完整镜像 + source=litellm
	g := byModel["gpt-4o"]
	require.Equal(t, int64(250000), g.PromptPricePerMillion, "2.5e-6 USD/token × 1e11 = 250000 毫分/1M")
	require.Equal(t, int64(1000000), g.CompletionPricePerMillion, "1e-5 × 1e11 = 1000000")
	require.NotNil(t, g.MaxInputTokens)
	require.Equal(t, int64(128000), *g.MaxInputTokens)
	require.NotNil(t, g.MaxOutputTokens)
	require.Equal(t, int64(16384), *g.MaxOutputTokens)
	require.NotNil(t, g.CacheReadPricePerMillion)
	require.Equal(t, int64(100000), *g.CacheReadPricePerMillion, "cache_read 1e-6 USD/token × 1e11 = 100000 毫分/1M")
	require.NotNil(t, g.CacheCreationPricePerMillion)
	require.Equal(t, int64(200000), *g.CacheCreationPricePerMillion, "cache_creation 2e-6 → 200000")
	require.NotNil(t, g.Provider)
	require.Equal(t, "openai", *g.Provider)
	require.NotNil(t, g.Mode)
	require.Equal(t, "chat", *g.Mode)
	require.NotNil(t, g.SupportsPromptCaching)
	require.True(t, *g.SupportsPromptCaching)
	require.Equal(t, domain.PricingSourceLitellm, g.Source)

	// raw 完整镜像：含未映射字段（rpm/supports_vision/字符价）且无字段丢失。
	require.NotNil(t, g.Raw, "raw 必须保存（含未映射字段）")
	var raw map[string]any
	require.NoError(t, json.Unmarshal(g.Raw, &raw))
	for _, k := range []string{"input_cost_per_token", "output_cost_per_token",
		"cache_read_input_token_cost", "cache_creation_input_token_cost",
		"max_input_tokens", "max_output_tokens", "litellm_provider", "mode",
		"supports_prompt_caching", "input_cost_per_character",
		"output_cost_per_character", "rpm", "supports_vision"} {
		_, ok := raw[k]
		require.True(t, ok, "raw 无字段丢失: %s", k)
	}
	require.Equal(t, float64(600), raw["rpm"], "raw 保留未映射字段 rpm")
	require.Equal(t, true, raw["supports_vision"], "raw 保留未映射字段 supports_vision")
	require.Equal(t, float64(1.0), raw["input_cost_per_character"], "raw 保留字符价字段")

	// claude-3-5-sonnet：max_output_tokens/cache 价/supports_prompt_caching
	// 缺失 → nil；provider/mode 提取
	c := byModel["claude-3-5-sonnet"]
	require.Equal(t, int64(300000), c.PromptPricePerMillion)
	require.Equal(t, int64(1500000), c.CompletionPricePerMillion)
	require.NotNil(t, c.MaxInputTokens)
	require.Equal(t, int64(200000), *c.MaxInputTokens)
	require.Nil(t, c.MaxOutputTokens, "缺失 → nil")
	require.Nil(t, c.CacheReadPricePerMillion, "cache 价缺失 → nil")
	require.Nil(t, c.CacheCreationPricePerMillion)
	require.NotNil(t, c.Provider)
	require.Equal(t, "anthropic", *c.Provider)
	require.Nil(t, c.SupportsPromptCaching, "supports_prompt_caching 缺失 → nil")

	// 四舍五入取整：6.123456789e-7 × 1e11 = 61234.56789 → 61235
	require.Equal(t, int64(61235), byModel["tiny-rounding"].PromptPricePerMillion)

	// null/0 max_tokens 与 cache 价 → nil
	nm := byModel["no-max-tokens"]
	require.Nil(t, nm.MaxInputTokens)
	require.Nil(t, nm.MaxOutputTokens)
	require.Nil(t, nm.CacheReadPricePerMillion, "cache 价 0 → nil（litellm 表 0 占位语义）")
	require.Nil(t, nm.CacheCreationPricePerMillion, "cache 价 null → nil")
}

// TestParseInvalidRowsSkipped 无效行全部跳过：
// 缺 output 价 / 0 价 / null / 负价 / 字符串类型 / 溢出数字 / 非对象行。
func TestParseInvalidRowsSkipped(t *testing.T) {
	res, err := Parse([]byte(litellmFixtureJSON), nil)
	require.NoError(t, err)
	skipped := map[string]bool{}
	for _, p := range res.Rows {
		skipped[p.Model] = true
	}
	for _, m := range []string{"missing-output", "zero-cost", "null-cost",
		"negative-cost", "string-cost", "overflow-cost", "not-an-object"} {
		require.False(t, skipped[m], "%s 应被跳过", m)
	}
}

// TestParseTopLevelError 顶层非对象（数组）→ 整体解析错误。
func TestParseTopLevelError(t *testing.T) {
	_, err := Parse([]byte(`[{"input_cost_per_token": 1e-06}]`), nil)
	require.Error(t, err)
	_, err = Parse([]byte(`not json`), nil)
	require.Error(t, err)
}

// TestParseEmpty 空对象 → 0 行 0 跳过。
func TestParseEmpty(t *testing.T) {
	res, err := Parse([]byte(`{}`), nil)
	require.NoError(t, err)
	require.Empty(t, res.Rows)
	require.Zero(t, res.Skipped)
}

// TestFetchHTTP 真实 HTTP 拉取（httptest server）：200 解析；非 200 → 错误；
// URL 非法 → 错误；超时上下文 → 错误。
func TestFetchHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(litellmFixtureJSON))
	}))
	defer srv.Close()

	f := NewFetcher(nil, nil)
	res, err := f.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	require.Len(t, res.Rows, 4)
	require.Equal(t, 7, res.Skipped)

	// 非 200
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer bad.Close()
	_, err = f.Fetch(context.Background(), bad.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "status 404")

	// 非法 URL
	_, err = f.Fetch(context.Background(), "://bad-url")
	require.Error(t, err)

	// 超时（上下文已取消）
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	_, err = f.Fetch(ctx, srv.URL)
	require.Error(t, err)
}

// matrixFixtureJSON Phase 5 矩阵字段提取 fixture（对齐 litellm 官方表实测形态）：
//   - gpt-5.6-sol：priority/flex 单价档 + above_flex 组（_above_272k_tokens_flex）
//     4 分量 + provider_specific_entry.fast ×2.0（→ 20000 万分数）
//   - azure/gpt-5.6-sol：above_priority 组（_above_272k_tokens_priority，
//     无 cache_creation —— 组内缺失分量 = nil）+ fast ×6.0（→ 60000）
//   - future-256k：above 基础组 256k 档（N 任意动态识别）
//   - multi-tier：多档并存（200k 有 cache_read、500k 无）→ 取含完整
//     prompt+completion 的最大 N
//   - noisy-model：干扰键（字符阶梯 *_cost_per_character_above_*、缓存 TTL
//     above_1hr、long_context_*）不得匹配
//   - bad-matrix：矩阵价 0/负 → 该档丢弃 → 全 nil
const matrixFixtureJSON = `{
  "gpt-5.6-sol": {
    "mode": "chat",
    "input_cost_per_token": 2e-06,
    "output_cost_per_token": 8e-06,
    "input_cost_per_token_priority": 3e-06,
    "output_cost_per_token_priority": 1.2e-05,
    "cache_read_input_token_cost_priority": 1e-06,
    "cache_creation_input_token_cost_priority": 2e-06,
    "input_cost_per_token_flex": 1.5e-06,
    "output_cost_per_token_flex": 6e-06,
    "cache_read_input_token_cost_flex": 5e-07,
    "cache_creation_input_token_cost_flex": 1e-06,
    "input_cost_per_token_above_272k_tokens_flex": 1.2e-06,
    "output_cost_per_token_above_272k_tokens_flex": 4.8e-06,
    "cache_read_input_token_cost_above_272k_tokens_flex": 4e-07,
    "cache_creation_input_token_cost_above_272k_tokens_flex": 8e-07,
    "provider_specific_entry": {"fast": 2.0},
    "max_input_tokens": 272000,
    "litellm_provider": "openai"
  },
  "azure/gpt-5.6-sol": {
    "mode": "chat",
    "input_cost_per_token": 2e-06,
    "output_cost_per_token": 8e-06,
    "input_cost_per_token_priority": 3e-06,
    "output_cost_per_token_priority": 1.2e-05,
    "input_cost_per_token_above_272k_tokens_priority": 1.8e-06,
    "output_cost_per_token_above_272k_tokens_priority": 7.2e-06,
    "cache_read_input_token_cost_above_272k_tokens_priority": 6e-07,
    "provider_specific_entry": {"fast": 6.0},
    "litellm_provider": "azure"
  },
  "future-256k": {
    "mode": "chat",
    "input_cost_per_token": 1e-06,
    "output_cost_per_token": 2e-06,
    "input_cost_per_token_above_256k_tokens": 9e-07,
    "output_cost_per_token_above_256k_tokens": 1.8e-06,
    "cache_read_input_token_cost_above_256k_tokens": 3e-07
  },
  "multi-tier": {
    "mode": "chat",
    "input_cost_per_token": 1e-06,
    "output_cost_per_token": 2e-06,
    "input_cost_per_token_above_200k_tokens": 9e-07,
    "output_cost_per_token_above_200k_tokens": 1.8e-06,
    "input_cost_per_token_above_500k_tokens": 8e-07,
    "output_cost_per_token_above_500k_tokens": 1.6e-06,
    "cache_read_input_token_cost_above_200k_tokens": 3e-07
  },
  "noisy-model": {
    "mode": "chat",
    "input_cost_per_token": 1e-06,
    "output_cost_per_token": 2e-06,
    "input_cost_per_character_above_1m_tokens": 1.0,
    "output_cost_per_character_above_1m_tokens": 2.0,
    "input_cost_per_token_above_1hr": 5e-06,
    "output_cost_per_token_above_1hr": 6e-06,
    "cache_read_input_token_cost_above_1hr": 7e-06,
    "long_context_input_cost_per_token": 3e-06,
    "long_context_output_cost_per_token": 4e-06
  },
  "bad-matrix": {
    "mode": "chat",
    "input_cost_per_token": 1e-06,
    "output_cost_per_token": 2e-06,
    "input_cost_per_token_above_128k_tokens": 0,
    "output_cost_per_token_above_128k_tokens": -1e-06,
    "input_cost_per_token_priority": 0,
    "provider_specific_entry": {"fast": 0}
  }
}`

// TestParseMatrixFields Phase 5 矩阵提取：priority/flex 单价档 + above 三组
// （动态阈值 N×1000，锚定精确 key 排除干扰）+ fast 万分数换算。矩阵价缺失/
// 无效不参与行有效性判定（行仍解析成功）。
func TestParseMatrixFields(t *testing.T) {
	res, err := Parse([]byte(matrixFixtureJSON), nil)
	require.NoError(t, err)
	require.Equal(t, 6, len(res.Rows), "矩阵 fixture 全部数值有效（矩阵价无效不影响行有效性）")

	byModel := map[string]*domain.Pricing{}
	for _, p := range res.Rows {
		byModel[p.Model] = p
	}

	// gpt-5.6-sol：priority/flex 单价换算 + above_flex 组 4 分量 + 共享阈值 + fast ×2.0
	g := byModel["gpt-5.6-sol"]
	require.Equal(t, int64(300000), *g.PriorityPromptPricePerMillion, "3e-6 × 1e11 = 300000 毫分/1M")
	require.Equal(t, int64(1200000), *g.PriorityCompletionPricePerMillion)
	require.Equal(t, int64(100000), *g.PriorityCacheReadPricePerMillion)
	require.Equal(t, int64(200000), *g.PriorityCacheCreationPricePerMillion)
	require.Equal(t, int64(150000), *g.FlexPromptPricePerMillion, "1.5e-6 → 150000")
	require.Equal(t, int64(600000), *g.FlexCompletionPricePerMillion)
	require.Equal(t, int64(50000), *g.FlexCacheReadPricePerMillion)
	require.Equal(t, int64(100000), *g.FlexCacheCreationPricePerMillion)
	require.Equal(t, int64(272000), *g.AboveThreshold, "272k → 阈值 272000 tokens")
	require.Nil(t, g.AbovePromptPricePerMillion, "无 above 基础组 → nil")
	require.Nil(t, g.AbovePriorityPromptPricePerMillion, "无 above_priority 组 → nil")
	require.Equal(t, int64(120000), *g.AboveFlexPromptPricePerMillion, "above_flex 1.2e-6 → 120000")
	require.Equal(t, int64(480000), *g.AboveFlexCompletionPricePerMillion)
	require.Equal(t, int64(40000), *g.AboveFlexCacheReadPricePerMillion)
	require.Equal(t, int64(80000), *g.AboveFlexCacheCreationPricePerMillion)
	require.Equal(t, int64(20000), *g.FastMultiplier, "fast ×2.0 → 20000 万分数")

	// azure/gpt-5.6-sol：above_priority 组 + fast ×6.0；组内缺失 cache_creation → nil
	a := byModel["azure/gpt-5.6-sol"]
	require.Equal(t, int64(272000), *a.AboveThreshold, "阈值跨组共享（maxN×1000）")
	require.Equal(t, int64(180000), *a.AbovePriorityPromptPricePerMillion)
	require.Equal(t, int64(720000), *a.AbovePriorityCompletionPricePerMillion)
	require.Equal(t, int64(60000), *a.AbovePriorityCacheReadPricePerMillion)
	require.Nil(t, a.AbovePriorityCacheCreationPricePerMillion, "azure 无 above_priority cache_creation → nil（实测形态）")
	require.Nil(t, a.AboveFlexPromptPricePerMillion, "无 above_flex 组 → nil")
	require.Equal(t, int64(60000), *a.FastMultiplier, "fast ×6.0 → 60000 万分数")

	// future-256k：above 基础组 256k（N 任意动态）
	f := byModel["future-256k"]
	require.Equal(t, int64(256000), *f.AboveThreshold)
	require.Equal(t, int64(90000), *f.AbovePromptPricePerMillion)
	require.Equal(t, int64(180000), *f.AboveCompletionPricePerMillion)
	require.Equal(t, int64(30000), *f.AboveCacheReadPricePerMillion)
	require.Nil(t, f.AboveCacheCreationPricePerMillion, "组内缺失分量 → nil")
	require.Nil(t, f.FastMultiplier)

	// multi-tier：200k/500k 并存 → 取含完整 prompt+completion 的最大 N（500k）
	mt := byModel["multi-tier"]
	require.Equal(t, int64(500000), *mt.AboveThreshold, "多档取最大 N")
	require.Equal(t, int64(80000), *mt.AbovePromptPricePerMillion)
	require.Equal(t, int64(160000), *mt.AboveCompletionPricePerMillion)
	require.Nil(t, mt.AboveCacheReadPricePerMillion, "500k 档无 cache_read → nil（200k 档的 cache_read 不串档）")

	// noisy-model：字符阶梯/above_1hr/long_context 全部不匹配
	n := byModel["noisy-model"]
	require.Nil(t, n.AboveThreshold, "干扰键不匹配 → 无分段")
	require.Nil(t, n.AbovePromptPricePerMillion)
	require.Nil(t, n.PriorityPromptPricePerMillion)
	require.Nil(t, n.FastMultiplier)

	// bad-matrix：0/负矩阵价丢弃 → 全 nil（行仍有效）
	bm := byModel["bad-matrix"]
	require.Equal(t, int64(100000), bm.PromptPricePerMillion, "基础价正常解析")
	require.Nil(t, bm.AboveThreshold, "above 全无效 → nil")
	require.Nil(t, bm.PriorityPromptPricePerMillion)
	require.Nil(t, bm.FastMultiplier)
}

// newFetchTestLogger warn 级文件 logger（Warn 断言用；与 flusher_test 同款
// 模式——zap OutputPaths 仅支持文件路径）。
func newFetchTestLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pricing-fetch-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New("warn", out)
	require.NoError(t, err)
	return logger, out
}

// TestExtractAboveMultiTierWarn A-P2-12 方案 A 可观测化：同模型同组多个合格
// above 档位并存（setGroup 丢弃低档——多档位当前按基础价计费）→ Warn 恰一条；
// 单档/无档/全无效档模型不告警。仅告警不改变提取结果（multi-tier 模型仍取
// 最大 N=500k，与 TestParseMatrixFields 断言一致）。
func TestExtractAboveMultiTierWarn(t *testing.T) {
	logger, out := newFetchTestLogger(t)
	res, err := Parse([]byte(matrixFixtureJSON), logger)
	require.NoError(t, err)
	require.Len(t, res.Rows, 6, "Warn 不影响解析结果/行有效性")

	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	// 恰一条：仅 multi-tier 模型 base 组 200k/500k 两档并存触发；gpt-5.6-sol
	// （flex 单档）/azure（priority 单档）/future-256k（base 单档）/
	// noisy-model（无档）/bad-matrix（全无效）均不告警。
	require.Equal(t, 1, strings.Count(string(b), "multi-tier above pricing dropped"))
	require.Contains(t, string(b), `"model":"multi-tier"`)
	require.Contains(t, string(b), `"group":"base"`)
	require.Contains(t, string(b), `"kept_tier_tokens":500000`)
}

// --- Task A：image 价独立判定（litellmEntry image 三字段扩展） ---

// imagePriceFixtureJSON 覆盖 image 价判定的全部形态（用户裁决 2026-08-12
// 按 mode 切分）：mode=image_generation 收——纯 image 价模型（gpt-image-2
// 官方形态：仅 image token 价无文本价；aiml 形态：仅 per-image 价）、文本价 +
// image 价双行并存；mode=chat 带视觉 image 字段不收（多模态视觉 token 价非
// 生图价）；mode 缺失跳过（宁漏勿错）；mode 命中但 image 价全无效（0/负/null
// → 无 image 行）、字段类型非法（字符串 → 整条目跳过）。
const imagePriceFixtureJSON = `{
  "gpt-image-2": {
    "input_cost_per_image_token": 8e-06,
    "output_cost_per_image_token": 3e-05,
    "max_input_tokens": 1000,
    "litellm_provider": "openai",
    "mode": "image_generation"
  },
  "aiml-image": {
    "output_cost_per_image": 0.054,
    "mode": "image_generation"
  },
  "dual-pricing": {
    "input_cost_per_token": 2.5e-06,
    "output_cost_per_token": 1e-05,
    "output_cost_per_image": 0.02,
    "mode": "image_generation"
  },
  "chat-with-vision": {
    "input_cost_per_token": 2.5e-06,
    "output_cost_per_token": 1e-05,
    "input_cost_per_image_token": 3e-06,
    "output_cost_per_image_token": 6e-06,
    "mode": "chat"
  },
  "no-mode-image": {
    "output_cost_per_image": 0.054
  },
  "embedding-token-priced": {
    "input_cost_per_token": 2.5e-06,
    "output_cost_per_token": 1e-05,
    "input_cost_per_image_token": 3e-06,
    "mode": "embedding"
  },
  "no-mode-token": {
    "input_cost_per_token": 2.5e-06,
    "output_cost_per_token": 1e-05
  },
  "invalid-image": {
    "input_cost_per_image_token": 0,
    "output_cost_per_image_token": -3e-05,
    "output_cost_per_image": null,
    "mode": "image_generation"
  },
  "string-image-cost": {
    "output_cost_per_image": "0.054",
    "mode": "image_generation"
  }
}`

// TestParseImageRows Task A 双行判定：image 价独立于文本价（任一 image 分量
// 有效 → image_price 行）；换算断言 ×1e11（token）vs ×1e5（per-image）；
// 全无效拒绝（Skipped 语义含 image 判定）；与 pricings 双行并存；
// FetchResult.ImageRows 通道。
func TestParseImageRows(t *testing.T) {
	res, err := Parse([]byte(imagePriceFixtureJSON), nil)
	require.NoError(t, err)

	// 两表按 mode 互斥（用户裁决）：image_generation 仅入 image_price（
	// dual-pricing 带文本价也不入 pricings）；chat 仅入 pricings（
	// chat-with-vision 带 image 字段也不入 image_price）；embedding/no-mode
	// 两表都不收 → Skipped 5（整条目跳过计一次）。
	require.Len(t, res.Rows, 1, "仅 chat-with-vision 入 pricings")
	require.Len(t, res.ImageRows, 3, "mode=image_generation 且 image 价有效的模型产 image 行")
	require.Equal(t, 5, res.Skipped, "invalid/string/no-mode-image/embedding/no-mode-token → skip")

	byModel := map[string]*domain.ImagePrice{}
	for _, p := range res.ImageRows {
		byModel[p.Model] = p
	}

	// gpt-image-2 官方形态：per-token ×1e11（8e-06 → 800,000；3e-05 →
	// 3,000,000）；无 per-image 价 → nil；raw 完整镜像
	g := byModel["gpt-image-2"]
	require.NotNil(t, g.InputImageTokenPricePerMillion)
	require.Equal(t, int64(800000), *g.InputImageTokenPricePerMillion, "8e-06 USD/token × 1e11 = 800000 毫分/1M")
	require.NotNil(t, g.OutputImageTokenPricePerMillion)
	require.Equal(t, int64(3000000), *g.OutputImageTokenPricePerMillion, "3e-05 × 1e11 = 3000000")
	require.Nil(t, g.OutputCostPerImageMilli, "无 per-image 价 → nil")
	require.Equal(t, domain.PricingSourceLitellm, g.Source)
	require.NotNil(t, g.Raw, "raw 完整镜像")
	var raw map[string]any
	require.NoError(t, json.Unmarshal(g.Raw, &raw))
	require.Equal(t, "openai", raw["litellm_provider"], "raw 保留元数据")

	// aiml 形态：0.054 USD/张 × 1e5 = 5400 毫分/张（per-image 换算系与 token
	// 价 ×1e11 不同——混用错 6 个数量级）
	a := byModel["aiml-image"]
	require.Nil(t, a.InputImageTokenPricePerMillion, "无 image token 价 → nil")
	require.NotNil(t, a.OutputCostPerImageMilli)
	require.Equal(t, int64(5400), *a.OutputCostPerImageMilli, "0.054 × 1e5 = 5400 毫分/张")

	// dual-pricing：mode=image_generation 仅入 image_price——带文本价也不入
	// pricings（两表按 mode 互斥）
	d := byModel["dual-pricing"]
	require.NotNil(t, d.OutputCostPerImageMilli)
	require.Equal(t, int64(2000), *d.OutputCostPerImageMilli, "0.02 × 1e5 = 2000 毫分/张")
	for _, p := range res.Rows {
		require.NotEqual(t, "dual-pricing", p.Model, "image_generation 模型不入 pricings")
	}

	// invalid-image：0/负/null → 无 image 行（不产出也不计双 skip）
	_, ok := byModel["invalid-image"]
	require.False(t, ok, "image 价全无效 → 无 image 行")
	_, ok = byModel["string-image-cost"]
	require.False(t, ok, "字段类型非法 → 无 image 行")

	// 用户裁决按 mode 切分：chat 模型带视觉 image 字段不收（多模态视觉 token
	// 价非生图价）——防 gpt-4o 等混入 image_price 表误放行 images 402 预检；
	// 其文本价照常产 pricings 行
	_, ok = byModel["chat-with-vision"]
	require.False(t, ok, "mode=chat 带 image 字段 → 不收（视觉 token 价非生图价）")
	var chatRows []*domain.Pricing
	for _, p := range res.Rows {
		if p.Model == "chat-with-vision" {
			chatRows = append(chatRows, p)
		}
	}
	require.Len(t, chatRows, 1, "mode=chat 文本价照常收")
	require.Equal(t, int64(250000), chatRows[0].PromptPricePerMillion)

	// mode 缺失 → 跳过（宁漏勿错，手动设价可补）
	_, ok = byModel["no-mode-image"]
	require.False(t, ok, "mode 缺失 → 跳过")

	// embedding 带 token 价：两表都不收（防混入 pricings 的 embedding 模型）
	_, ok = byModel["embedding-token-priced"]
	require.False(t, ok, "mode=embedding 不入 image_price")
	var embRows []*domain.Pricing
	for _, p := range res.Rows {
		if p.Model == "embedding-token-priced" {
			embRows = append(embRows, p)
		}
	}
	require.Empty(t, embRows, "mode=embedding 不入 pricings（带 token 价也不收）")

	// 无 mode 带 token 价：两表都不收（宁漏勿错，手动设价可补）
	_, ok = byModel["no-mode-token"]
	require.False(t, ok, "无 mode 不入 image_price")
	for _, p := range res.Rows {
		require.NotEqual(t, "no-mode-token", p.Model, "无 mode 不入 pricings")
	}
}

// TestParseImageRowsNoImageFields 无 image 字段的条目：ImageRows 为空、Skipped
// 语义不变（原 fixture 全部无 image 价 → 4 行 7 跳过，text 判定不受影响）。
func TestParseImageRowsNoImageFields(t *testing.T) {
	res, err := Parse([]byte(litellmFixtureJSON), nil)
	require.NoError(t, err)
	require.Len(t, res.Rows, 4, "文本价判定不变")
	require.Empty(t, res.ImageRows, "无 image 字段 → 无 image 行")
	require.Equal(t, 7, res.Skipped, "Skipped 语义含 image 判定但无 image 字段条目不计额外 skip")
}
