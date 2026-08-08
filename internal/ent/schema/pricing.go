package schema

import (
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
type Pricing struct{ ent.Schema }

func (Pricing) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("model").Unique(),
		field.Int64("prompt_price_per_million"),
		field.Int64("completion_price_per_million"),
		field.Int64("max_input_tokens").Optional().Nillable(),
		field.Int64("max_output_tokens").Optional().Nillable(),
		field.Enum("source").Values("litellm", "manual"),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}
