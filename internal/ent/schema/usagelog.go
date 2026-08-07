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
		field.Int64("latency_ms").Default(0),
		field.Int64("prompt_tokens").Default(0),
		field.Int64("completion_tokens").Default(0),
		field.Int64("total_tokens").Default(0),
		field.Int64("cache_read_tokens").Default(0),
		field.Int64("cache_creation_tokens").Default(0),
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
