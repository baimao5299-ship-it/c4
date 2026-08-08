package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
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
    "output_cost_per_token": 1e-06
  },
  "no-max-tokens": {
    "input_cost_per_token": 1e-06,
    "output_cost_per_token": 2e-06,
    "max_input_tokens": null,
    "max_output_tokens": 0,
    "cache_read_input_token_cost": 0,
    "cache_creation_input_token_cost": null
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
	res, err := Parse([]byte(litellmFixtureJSON))
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
	res, err := Parse([]byte(litellmFixtureJSON))
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
	_, err := Parse([]byte(`[{"input_cost_per_token": 1e-06}]`))
	require.Error(t, err)
	_, err = Parse([]byte(`not json`))
	require.Error(t, err)
}

// TestParseEmpty 空对象 → 0 行 0 跳过。
func TestParseEmpty(t *testing.T) {
	res, err := Parse([]byte(`{}`))
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

	f := NewFetcher(nil)
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
    "input_cost_per_token": 1e-06,
    "output_cost_per_token": 2e-06,
    "input_cost_per_token_above_256k_tokens": 9e-07,
    "output_cost_per_token_above_256k_tokens": 1.8e-06,
    "cache_read_input_token_cost_above_256k_tokens": 3e-07
  },
  "multi-tier": {
    "input_cost_per_token": 1e-06,
    "output_cost_per_token": 2e-06,
    "input_cost_per_token_above_200k_tokens": 9e-07,
    "output_cost_per_token_above_200k_tokens": 1.8e-06,
    "input_cost_per_token_above_500k_tokens": 8e-07,
    "output_cost_per_token_above_500k_tokens": 1.6e-06,
    "cache_read_input_token_cost_above_200k_tokens": 3e-07
  },
  "noisy-model": {
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
	res, err := Parse([]byte(matrixFixtureJSON))
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
