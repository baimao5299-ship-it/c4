package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type UsageLog struct{ ent.Schema }

func (UsageLog) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("request_id"),
		field.Int64("group_id").Optional().Nillable(),
		field.Int64("account_id").Optional().Nillable(),
		field.Int64("template_id").Optional().Nillable(),
		field.Int64("user_id").Optional().Nillable(), // 鉴权 key 归属用户（context 传递，0 = 无）
		field.Int64("key_id").Optional().Nillable(),  // 鉴权 key（context 传递，0 = 无）
		field.String("model").Default(""),
		field.String("mapped_model").Optional().Nillable(),
		field.Enum("format").
			Values("openai-chat", "openai-responses", "anthropic"),
		field.Int("status_code").Default(0),
		field.String("error_type").Default("none"),
		// 错误文本（部署故障修复）：连接级 err.Error() / 4xx+ 上游 body，域内
		// 截断 500 字符（domain.TruncateErrMsg）；NULL = 成功路径无错误文本。
		field.String("error_message").Optional().Nillable(),
		field.Int64("latency_ms").Default(0),
		field.Int64("input_tokens").Default(0),
		field.Int64("output_tokens").Default(0),
		field.Int64("total_tokens").Default(0),
		field.Int64("cache_read_tokens").Default(0),
		field.Int64("cache_creation_tokens").Default(0),
		// 计费列（Phase 5）：cost 毫分（1 USD = 100,000 毫分）；billing_tier
		// 请求 service_tier 归一化值（priority/flex/fast/auto；nil = 未计费路径）；
		// above_hit 任一分量超 above_threshold 命中分段；overdraft 本次扣费透支
		// （负余额）。错误请求（402/4xx）cost = 0。
		field.Int64("cost").Default(0),
		field.String("billing_tier").Optional().Nillable(),
		field.Bool("above_hit").Default(false),
		field.Bool("overdraft").Default(false),
		field.Time("created_at").Default(time.Now),
	}
}

func (UsageLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at"),
		index.Fields("group_id", "created_at"),
		index.Fields("account_id", "created_at"),
		index.Fields("user_id", "created_at"),
		index.Fields("key_id", "created_at"),
	}
}
