package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type UsageStat struct{ ent.Schema }

func (UsageStat) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Time("bucket_time"),
		field.Int64("group_id").Default(0), // 0 = 无（唯一索引需要非 NULL）
		field.Int64("account_id").Default(0),
		field.Int64("template_id").Default(0),
		field.String("model").Default(""),
		field.Bool("is_error").Default(false),
		field.Int64("request_count").Default(0),
		field.Int64("error_count").Default(0),
		field.Int64("prompt_tokens").Default(0),
		field.Int64("completion_tokens").Default(0),
		field.Int64("total_tokens").Default(0),
		field.Int64("total_latency_ms").Default(0),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (UsageStat) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("bucket_time"),
		index.Fields("bucket_time", "group_id", "account_id", "template_id", "model", "is_error").Unique(),
	}
}
