package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

type Template struct{ ent.Schema }

func (Template) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("name").Unique(),
		field.String("base_url"),
		field.JSON("supported_formats", []string{}),       // 格式数组
		field.JSON("models", []string{}),
		field.JSON("format_models", map[string][]string{}), // format -> model 列表
		field.JSON("model_mapping", map[string]string{}),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Template) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("accounts", Account.Type),
	}
}
