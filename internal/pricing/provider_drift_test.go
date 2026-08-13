// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// providerDriftFixtureJSON 模拟 litellm 官方价格表的主流厂商样本（与 openapi
// components/schemas/Provider 枚举一一对应，mode=chat 且带有效文本价 → 每条目
// 产 pricings 行 + provider 提取）。手写而非由 openapi 生成——测试的价值在于
// **防 enum 与 litellm 实际字符串漂移**：litellm 更新加新厂商时，此处 fixture
// 需同步补样本（或扩 openapi enum），缺一即断言失败。
const providerDriftFixtureJSON = `{
  "drift-openai": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "openai" },
  "drift-anthropic": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "anthropic" },
  "drift-azure": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "azure" },
  "drift-vertex-ai": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "vertex_ai" },
  "drift-bedrock": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "bedrock" },
  "drift-deepseek": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "deepseek" },
  "drift-mistral": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "mistral" },
  "drift-cohere": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "cohere" },
  "drift-xai": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "xai" },
  "drift-openrouter": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "openrouter" },
  "drift-groq": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "groq" },
  "drift-together-ai": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "together_ai" },
  "drift-fireworks-ai": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "fireworks_ai" },
  "drift-replicate": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "replicate" },
  "drift-huggingface": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "huggingface" },
  "drift-moonshot": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "moonshot" },
  "drift-zhipu": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "zhipu" },
  "drift-baidu": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "baidu" },
  "drift-alibaba": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "alibaba" },
  "drift-meta": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "meta" },
  "drift-nvidia": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "nvidia" },
  "drift-cerebras": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "cerebras" },
  "drift-perplexity": { "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06, "mode": "chat", "litellm_provider": "perplexity" }
}`

// TestProviderEnumDrift openapi Provider enum 与拉取样本一致性（防 enum 与
// litellm 字符串漂移）：openapi components/schemas/Provider 的每个枚举值必须
// 出现在拉取样本（providerDriftFixtureJSON 模拟 litellm 官方表主流厂商）中。
// litellm_provider 动态——litellm 更新加新厂商时，扩 openapi enum 与 fixture
// 样本须同步（DB 筛选为自由字符串等值，不受 enum 约束，此断言只防前端下拉框
// 与实际字符串漂移）。
func TestProviderEnumDrift(t *testing.T) {
	// 读 openapi Provider enum（测试 cwd = 包目录）
	b, err := os.ReadFile("../../openapi/openapi.yaml")
	require.NoError(t, err, "读取 openapi/openapi.yaml")
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `yaml:"enum"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	require.NoError(t, yaml.Unmarshal(b, &doc))
	enum, ok := doc.Components.Schemas["Provider"]
	require.True(t, ok, "openapi components/schemas/Provider 存在")
	require.NotEmpty(t, enum.Enum, "Provider enum 非空")

	// 拉取样本：三表合一收集实际提取到的 provider 字符串
	res, err := Parse([]byte(providerDriftFixtureJSON), nil)
	require.NoError(t, err)
	require.Len(t, res.Rows, len(enum.Enum), "fixture 每枚举值一条有效行")
	samples := map[string]bool{}
	for _, p := range res.Rows {
		require.NotNil(t, p.Provider, "fixture 条目必须带 litellm_provider")
		samples[*p.Provider] = true
	}
	require.Len(t, samples, len(enum.Enum), "fixture 样本互不重复")

	// enum ⊆ 样本：enum 值漂移（litellm 字符串改名/拼错）→ 断言失败
	for _, v := range enum.Enum {
		require.True(t, samples[v], "Provider enum %q 必须出现在拉取样本中（防 enum 与 litellm 字符串漂移）", v)
	}
}
