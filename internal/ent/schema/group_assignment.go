package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// GroupAssignment private 组的授予对象（用户）：联合唯一 (group_id, user_id)。
type GroupAssignment struct{ ent.Schema }

func (GroupAssignment) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("group_id"),
		field.Int64("user_id"),
		field.Time("created_at").Default(time.Now),
	}
}

func (GroupAssignment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", Group.Type).
			Ref("assignments").
			Field("group_id").
			Unique().
			Required(),
		edge.From("user", User.Type).
			Ref("group_assignments").
			Field("user_id").
			Unique().
			Required(),
	}
}

func (GroupAssignment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "user_id").Unique(),
	}
}
