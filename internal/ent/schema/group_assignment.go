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
		// 用户在该组的专属价格倍率（万分数，T3.5 修正：按组——用户在不同组
		// 可有不同倍率）；nil = 未设置 → 用组倍率（区分"设了 ×1"与"未设置"）。
		// 0 = 免费；上限 100000 = ×10。
		field.Int("price_multiplier").Optional().Nillable(),
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
