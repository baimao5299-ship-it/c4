package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// RedemptionUse 兑换审计：全量留痕（含兑换时的 value/resource_expires_at 快照）。
// UNIQUE(code_id, user_id)：同用户不可重复兑换同一码，DB 兜底幂等（重复兑换 → 409）。
type RedemptionUse struct{ ent.Schema }

func (RedemptionUse) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("code_id"),
		field.Int64("user_id"),
		field.Int64("value"),                                    // 兑换时的值快照（最小单位）
		field.Int64("group_id").Optional().Nillable(),           // scoped code group snapshot; NULL = global/legacy
		field.Time("resource_expires_at").Optional().Nillable(), // 资源到期快照
		field.Time("created_at").Default(time.Now),
	}
}

func (RedemptionUse) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("code", RedemptionCode.Type).
			Ref("uses").
			Field("code_id").
			Unique().
			Required(),
	}
}

func (RedemptionUse) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("code_id", "user_id").Unique(),
		// user_id 前缀索引：ListUsesByUser + Count 按 user 过滤免 (code_id,user_id)
		// 复合索引无法覆盖的 user_id 前缀双扫（写低频，写放大可忽略）。
		index.Fields("user_id"),
	}
}
