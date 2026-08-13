package schema

import (
	"encoding/json"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// ImagePrice 图片生成价格表（Task A 数据面；images 端点计费价格来源）：
//   - 单位：image token 价 = 毫分/1M image tokens（1 USD = 100,000 毫分，
//     litellm per-token USD ×1e11）；per-image 价 = 毫分/张（litellm
//     output_cost_per_image USD ×1e5——**与 token 价不同换算系、不同单位，
//     计费不走 /1e6 除法**）
//   - 三价格列全 nullable：nil = 未配置该分量（行有效性 = 至少一个分量非 nil，
//     DB 无法约束，应用层拒绝全 nil 落行/设价）
//   - source 行级互斥优先级 manual > litellm：拉取 upsert 带 WHERE 条件
//     （image_price.source != 'manual'）永不覆盖手动价；手动设价强制 source=manual
//     可接管已存在的 litellm 行（对齐 pricings 同款机制）
//   - raw 为 litellm 原始条目完整镜像（JSONB）；manual 行恒为 NULL
//   - 无 deleted_at（对齐 pricings：价格表删除语义 = 移除手动覆盖，非保留历史）
type ImagePrice struct{ ent.Schema }

func (ImagePrice) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("model").Unique(), // 与 pricings.model 同口径（模型名）
		field.Int64("input_image_token_price_per_million").Optional().Nillable(),
		field.Int64("output_image_token_price_per_million").Optional().Nillable(),
		field.Int64("output_cost_per_image_milli").Optional().Nillable(),
		field.String("provider").Optional().Nillable(), // litellm_provider（litellm 行才有；manual 行 nil）
		field.JSON("raw", json.RawMessage{}).Optional(),
		field.Enum("source").Values("litellm", "manual"),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}
