package schema

import (
	"encoding/json"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// FunctionPrice 按单元计费功能类价格表（search 起，audio/video 等未来 per-unit
// 端点复用；对齐 image_price 形态）：
//   - 单位：price_per_call = 毫分/单元（1 USD = 100,000 毫分；litellm
//     input_cost_per_query USD ×1e5 → 毫分/次；未来 audio 每秒等按各自换算）
//   - price_per_call 可空：nil = 无按单元价（行有效性 = 非 nil，应用层拒绝
//     全 nil 落行/设价——对齐 image_price 至少一价语义）
//   - source 行级互斥优先级 manual > litellm：拉取 upsert 带 WHERE 条件
//     （function_price.source != 'manual'）永不覆盖手动价；手动设价强制
//     source=manual 可接管已存在的 litellm 行（对齐 pricings/image_price 同款机制）
//   - raw 为 litellm 原始条目完整镜像（JSONB）；manual 行恒为 NULL
//   - 无 deleted_at（对齐 pricings：价格表删除语义 = 移除手动覆盖，非保留历史）
type FunctionPrice struct{ ent.Schema }

func (FunctionPrice) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("model").Unique(), // 模型/功能标识（litellm search 模型名 或 codex-search 固定标识）
		field.Int64("price_per_call").Optional().Nillable(),
		field.JSON("raw", json.RawMessage{}).Optional(),
		field.Enum("source").Values("litellm", "manual"),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}
