package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

type Account struct{ ent.Schema }

func (Account) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("name"),
		field.Int64("template_id"),
		field.String("upstream_key"),
		field.Enum("status").
			Values("active", "unhealthy", "429", "disabled").
			Default("active"),
		field.Time("cooldown_until").Optional().Nillable(),
		field.Int("weight").Default(100),
		field.Int("max_concurrency").Default(8),
		field.String("last_error").Optional().Nillable(),
		field.Time("last_used_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Account) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("template", Template.Type).
			Ref("accounts").
			Field("template_id").
			Unique().
			Required(),
		edge.To("groups", Group.Type),
	}
}
