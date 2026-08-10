package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Key 客户端 API key：独立表（重建 group 内嵌 key 语义，不向后兼容）。
// 多 key 选组、key 级轮换/禁用/并发上限/额度（quota/quota_used 后扣模型）。
type Key struct{ ent.Schema }

func (Key) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("user_id"),
		field.Int64("group_id"),
		field.String("name"),
		field.String("key_hash").Unique(),
		field.String("key_prefix"),
		field.Enum("status").Values("active", "disabled").Default("active"),
		field.Int("max_concurrency").Default(0), // 0 = 不限
		field.Int64("quota").Default(0),         // 累计 token 上限；0 = 不限（HasQuota 短路）
		field.Int64("quota_used").Default(0),    // 已消耗（后扣；无额度 key 恒 0）
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional().Nillable(), // 软删除时间戳（nil = 存活）；null 语义 = 未删除
		field.Time("created_at").Default(time.Now),
	}
}

func (Key) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("keys").
			Field("user_id").
			Unique().
			Required(),
		edge.From("group", Group.Type).
			Ref("keys").
			Field("group_id").
			Unique().
			Required(),
	}
}
