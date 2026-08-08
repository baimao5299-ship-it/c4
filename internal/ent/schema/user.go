package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// User 用户即顶层（无租户实体）。标识 = 邮箱（无 username 字段，与 sub2api
// 数据可迁移）；密码哈希 = bcrypt DefaultCost(10)，sub2api 同参数存量 hash
// 直接可验证。balance（最小单位）本轮只建模型（管理面可读写），扣费逻辑
// Phase 5 计费。
type User struct{ ent.Schema }

func (User) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("email").Unique(),
		field.String("password_hash"),
		field.Enum("role").Values("platform_admin", "user").Default("user"),
		field.Enum("status").Values("active", "disabled").Default("active"),
		field.Int("max_concurrency").Default(0), // 0 = 不限
		field.Int64("balance").Default(0),       // 最小单位；Phase 5 扣费
		// 万分数（T3.5 价格倍率）；nil = 未设置 → 用组倍率（区分"设了 ×1"与
		// "未设置"的关键——用户覆盖组语义）。
		field.Int("price_multiplier").Optional().Nillable(),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("keys", Key.Type),
		edge.To("temp_balances", TempBalance.Type),
		edge.To("group_assignments", GroupAssignment.Type),
	}
}
