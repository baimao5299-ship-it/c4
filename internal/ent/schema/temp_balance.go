package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// TempBalance 临时额度：每笔独立行、独立到期（多笔不同到期共存）。
// 可用合计 = 未过期 SUM；扣费 FEFO（最早到期先扣，Phase 5）。
// 不在 users 表放聚合列（单一事实源）；不用 JSONB 数组（sub2api 坏味道）。
type TempBalance struct{ ent.Schema }

func (TempBalance) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("user_id"),
		field.Int64("amount"),                          // 最小单位；可负（扣减记录）
		field.Int64("group_id").Optional().Nillable(),  // NULL = global/legacy; otherwise exact usage group
		field.Time("expires_at").Optional().Nillable(), // nil = 永久
		field.String("note").Optional().Nillable(),
		field.Time("created_at").Default(time.Now),
	}
}

func (TempBalance) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("temp_balances").
			Field("user_id").
			Unique().
			Required(),
	}
}

// Indexes 计费 FEFO 查询（user_id EQ + ORDER BY expires_at，permanent 最后）
// 的索引载体：user_id 收敛到用户子集，expires_at 序直接满足排序（PG ASC
// 默认 NULLS LAST，与 nil=永久最后注释语义一致）。
func (TempBalance) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "expires_at"),
		index.Fields("user_id", "group_id", "expires_at"),
	}
}
