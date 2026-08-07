package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// Setting 类型化配置：key 唯一、type 枚举（switch|number|string）、value
// 字符串存储。signup_enabled 注册开关等。
type Setting struct{ ent.Schema }

func (Setting) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("key").Unique(),
		field.Enum("type").Values("switch", "number", "string"),
		field.String("value").Default(""),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}
