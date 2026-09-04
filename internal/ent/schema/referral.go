package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Referral records the immutable inviter selected during registration.
type Referral struct{ ent.Schema }

func (Referral) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("inviter_id"),
		field.Int64("invitee_id").Unique(),
		field.Time("created_at").Default(time.Now),
	}
}

func (Referral) Indexes() []ent.Index {
	return []ent.Index{index.Fields("inviter_id", "created_at")}
}
