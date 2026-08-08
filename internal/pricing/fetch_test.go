package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
)

// litellmFixtureJSON 模拟 litellm 官方价格表结构（model_name → 行）：
// 正常行（含 max_tokens/未知字段）+ 无效行（缺 output 价 / 0 价 / null /
// 负价 / 字符串类型 / 溢出数字 / 非对象行）。
const litellmFixtureJSON = `{
  "gpt-4o": {
    "input_cost_per_token": 2.5e-06,
    "output_cost_per_token": 1e-05,
    "max_input_tokens": 128000,
    "max_output_tokens": 16384,
    "input_cost_per_character": 1.0,
    "output_cost_per_character": 2.0,
    "litellm_provider": "openai"
  },
  "claude-3-5-sonnet": {
    "input_cost_per_token": 3e-06,
    "output_cost_per_token": 1.5e-05,
    "max_input_tokens": 200000
  },
  "tiny-rounding": {
    "input_cost_per_token": 6.123456789e-07,
    "output_cost_per_token": 1e-06
  },
  "no-max-tokens": {
    "input_cost_per_token": 1e-06,
    "output_cost_per_token": 2e-06,
    "max_input_tokens": null,
    "max_output_tokens": 0
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
// 1500000；6.123456789e-7 → 61235（round）；max_tokens roundtrip（含 null/0 → nil）。
func TestParseValidRows(t *testing.T) {
	res, err := Parse([]byte(litellmFixtureJSON))
	require.NoError(t, err)
	require.Equal(t, 4, len(res.Rows), "4 个数值有效行")
	require.Equal(t, 7, res.Skipped, "7 个无效行跳过")

	byModel := map[string]*domain.Pricing{}
	for _, p := range res.Rows {
		byModel[p.Model] = p
	}

	// gpt-4o：换算精确 + max_tokens roundtrip + 未知字段忽略 + source=litellm
	g := byModel["gpt-4o"]
	require.Equal(t, int64(250000), g.PromptPricePerMillion, "2.5e-6 USD/token × 1e11 = 250000 毫分/1M")
	require.Equal(t, int64(1000000), g.CompletionPricePerMillion, "1e-5 × 1e11 = 1000000")
	require.NotNil(t, g.MaxInputTokens)
	require.Equal(t, int64(128000), *g.MaxInputTokens)
	require.NotNil(t, g.MaxOutputTokens)
	require.Equal(t, int64(16384), *g.MaxOutputTokens)
	require.Equal(t, domain.PricingSourceLitellm, g.Source)

	// claude-3-5-sonnet：max_output_tokens 缺失 → nil
	c := byModel["claude-3-5-sonnet"]
	require.Equal(t, int64(300000), c.PromptPricePerMillion)
	require.Equal(t, int64(1500000), c.CompletionPricePerMillion)
	require.NotNil(t, c.MaxInputTokens)
	require.Equal(t, int64(200000), *c.MaxInputTokens)
	require.Nil(t, c.MaxOutputTokens, "缺失 → nil")

	// 四舍五入取整：6.123456789e-7 × 1e11 = 61234.56789 → 61235
	require.Equal(t, int64(61235), byModel["tiny-rounding"].PromptPricePerMillion)

	// null/0 max_tokens → nil
	nm := byModel["no-max-tokens"]
	require.Nil(t, nm.MaxInputTokens)
	require.Nil(t, nm.MaxOutputTokens)
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
