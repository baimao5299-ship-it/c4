package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ReferralReward is the immutable, idempotent rebate ledger. Pending rewards
// become claimable after available_at and are credited only by an explicit claim.
type ReferralReward struct{ ent.Schema }

func (ReferralReward) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("inviter_id"),
		field.Int64("invitee_id"),
		field.Enum("source_type").Values("redemption", "admin_credit"),
		field.String("source_id"),
		field.String("idempotency_key").Unique(),
		field.Int64("base_amount"),
		field.Int("rate_bps").Default(500),
		field.Int64("reward_amount"),
		field.Enum("status").Values("pending", "credited", "reversed").Default("pending"),
		field.Time("available_at"),
		field.Time("credited_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now),
	}
}

func (ReferralReward) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("inviter_id", "status", "available_at"),
		index.Fields("invitee_id", "created_at"),
	}
}
