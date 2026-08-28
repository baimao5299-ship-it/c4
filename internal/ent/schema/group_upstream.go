package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// GroupUpstream stores the policy and member-local runtime state for one
// upstream in one group. The explicit relation avoids copying endpoint
// credentials and allows the same upstream to use different routing settings
// in different groups.
type GroupUpstream struct{ ent.Schema }

func (GroupUpstream) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("group_id"),
		field.Int64("upstream_id"),
		field.Int("weight").Default(100),
		field.Int("priority").Default(0),
		field.Int("max_concurrency").Default(8),
		field.Bool("enabled").Default(true),
		field.Time("cooldown_until").Optional().Nillable(),
		field.Int("failure_streak").Default(0),
		field.String("last_error").Optional().Nillable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("created_at").Default(time.Now),
	}
}

func (GroupUpstream) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", Group.Type).
			Ref("upstream_members").
			Field("group_id").
			Unique().
			Required(),
		edge.From("upstream", Upstream.Type).
			Ref("group_members").
			Field("upstream_id").
			Unique().
			Required(),
	}
}

func (GroupUpstream) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "upstream_id").Unique(),
		index.Fields("group_id", "priority", "id"),
	}
}
