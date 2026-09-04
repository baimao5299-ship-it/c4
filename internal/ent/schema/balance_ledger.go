package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// BalanceLedger preserves the exact before/after balance for every top-up and
// referral claim. Usage debits remain in usage_logs, their existing ledger.
type BalanceLedger struct{ ent.Schema }

func (BalanceLedger) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("user_id"),
		field.Enum("kind").Values("redemption", "admin_credit", "referral_claim"),
		field.String("source_id"),
		field.String("note").Optional().Nillable(),
		field.String("idempotency_key").Unique(),
		field.Int64("delta"),
		field.Int64("balance_before"),
		field.Int64("balance_after"),
		field.Int64("actor_user_id").Optional().Nillable(),
		field.Time("created_at").Default(time.Now),
	}
}

func (BalanceLedger) Indexes() []ent.Index {
	return []ent.Index{index.Fields("user_id", "created_at")}
}
