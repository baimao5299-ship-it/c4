package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Group 平台容量池（无内嵌 key 语义——Phase 3a 重建为独立 keys 表）：
// visibility(public|private)；private 授予对象 = 用户（group_assignments）。
type Group struct{ ent.Schema }

func (Group) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("name").Unique(),
		field.Enum("visibility").Values("public", "private").Default("public"),
		field.Int("price_multiplier").Default(10000), // 万分数（T3.5 价格倍率）：组默认 ×1；0 = 免费
		// protocol_convert 分组级协议转换（只补差，W5 消费）：off/chat_to_resp/
		// mess_to_resp/resp_to_mess/chat_to_mess；枚举校验在 service 层。
		field.String("protocol_convert").Default("off"),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional().Nillable(), // 软删除时间戳（nil = 存活）；null 语义 = 未删除
		field.Time("created_at").Default(time.Now),
	}
}

func (Group) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("accounts", Account.Type).Ref("groups"),
		edge.To("keys", Key.Type),
		edge.To("assignments", GroupAssignment.Type),
	}
}
