package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

type Rule struct{ ent.Schema }

func (Rule) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("name").Unique(),
		field.Bool("enabled").Default(true),
		field.Int("priority").Unique(),
		field.JSON("when", map[string]any{}),
		field.JSON("then", map[string]any{}),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}
