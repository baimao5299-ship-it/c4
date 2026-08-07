package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Group 平台容量池（无内嵌 key 语义——Phase 3a 重建为独立 keys 表）：
// visibility(public|private)；private 授予对象 = 用户（group_assignments）。
type Group struct{ ent.Schema }

func (Group) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("name").Unique(),
		field.Enum("visibility").Values("public", "private").Default("public"),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Group) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("accounts", Account.Type).Ref("groups"),
		edge.To("keys", Key.Type),
		edge.To("assignments", GroupAssignment.Type),
	}
}
