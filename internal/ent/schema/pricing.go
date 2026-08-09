package schema

import (
	"encoding/json"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// Pricing 模型价格表（Phase 5 计费价格来源）：
//   - 单位：毫分 / 1M tokens（1 USD = 100,000 毫分 = 10⁻⁵ USD 精度）
//   - source 行级互斥优先级 manual > litellm：拉取 upsert 带 WHERE 条件
//     （pricing.source != 'manual'）永不覆盖手动价；手动设价强制 source=manual
//     可接管已存在的 litellm 行
//   - max_input/output_tokens 为 litellm 官方表自带上下文窗口，未来上下文校验用
//   - cache_read/creation_price_per_million 为 litellm 官方表自带缓存价（USD/
//     token → 毫分/1M；与 litellm 表一致可缺失/为 0 → nil = 无缓存计费）
//   - provider/mode/supports_prompt_caching 为 litellm 官方表自带显式字段
//   - 矩阵价（Phase 5，挡位归属定稿）：priority/flex 各 4 价为 service_tier
//     单价替换档（**OpenAI 专属**：gpt-5 系列 priority 价、gpt-5.6-sol flex
//     价）；above 三组 12 价为上下文超阈值分段价（基础/priority/flex，对齐官方
//     表 tier 变体，如 gpt-5.6-sol 的 _above_272k_tokens_flex、azure 的
//     _above_272k_tokens_priority）；above_threshold = 分段阈值（tokens，litellm
//     _above_{N}k 动态提取）；fast_multiplier = **Anthropic 专属**（claude 系列
//     Fast Mode 整单倍率，万分数，20000 = ×2.0，fast 挡计费 = 基础价 × 此倍率，
//     源自 provider_specific_entry.fast）。基础 4 价与 above 三组 12 价通用。
//     全部矩阵价缺失 = nil（无该档价，计费回退基础价/不涨价），不参与行有效性
//     判定
//   - raw 为 litellm 原始条目完整镜像（JSONB，含未映射字段，如 rpm/supports_vision）；
//     manual 行恒为 NULL（手动价不写 raw）
type Pricing struct{ ent.Schema }

func (Pricing) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("model").Unique(),
		field.Int64("prompt_price_per_million"),
		field.Int64("completion_price_per_million"),
		field.Int64("max_input_tokens").Optional().Nillable(),
		field.Int64("max_output_tokens").Optional().Nillable(),
		field.Int64("cache_read_price_per_million").Optional().Nillable(),
		field.Int64("cache_creation_price_per_million").Optional().Nillable(),
		// 矩阵价：priority/flex 单价替换档 4+4；above 分段三组 12（基础/priority/flex）。
		field.Int64("priority_prompt_price_per_million").Optional().Nillable(),
		field.Int64("priority_completion_price_per_million").Optional().Nillable(),
		field.Int64("priority_cache_read_price_per_million").Optional().Nillable(),
		field.Int64("priority_cache_creation_price_per_million").Optional().Nillable(),
		field.Int64("flex_prompt_price_per_million").Optional().Nillable(),
		field.Int64("flex_completion_price_per_million").Optional().Nillable(),
		field.Int64("flex_cache_read_price_per_million").Optional().Nillable(),
		field.Int64("flex_cache_creation_price_per_million").Optional().Nillable(),
		field.Int64("above_threshold").Optional().Nillable(),
		field.Int64("above_prompt_price_per_million").Optional().Nillable(),
		field.Int64("above_completion_price_per_million").Optional().Nillable(),
		field.Int64("above_cache_read_price_per_million").Optional().Nillable(),
		field.Int64("above_cache_creation_price_per_million").Optional().Nillable(),
		field.Int64("above_priority_prompt_price_per_million").Optional().Nillable(),
		field.Int64("above_priority_completion_price_per_million").Optional().Nillable(),
		field.Int64("above_priority_cache_read_price_per_million").Optional().Nillable(),
		field.Int64("above_priority_cache_creation_price_per_million").Optional().Nillable(),
		field.Int64("above_flex_prompt_price_per_million").Optional().Nillable(),
		field.Int64("above_flex_completion_price_per_million").Optional().Nillable(),
		field.Int64("above_flex_cache_read_price_per_million").Optional().Nillable(),
		field.Int64("above_flex_cache_creation_price_per_million").Optional().Nillable(),
		field.Int64("fast_multiplier").Optional().Nillable(), // 万分数（20000 = ×2.0）
		field.String("provider").Optional().Nillable(),
		field.String("mode").Optional().Nillable(),
		field.Bool("supports_prompt_caching").Optional().Nillable(),
		field.JSON("raw", json.RawMessage{}).Optional(),
		field.Enum("source").Values("litellm", "manual"),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}
