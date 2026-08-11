package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type UsageLog struct{ ent.Schema }

func (UsageLog) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("request_id"),
		field.Int64("group_id").Optional().Nillable(),
		field.Int64("account_id").Optional().Nillable(),
		field.Int64("template_id").Optional().Nillable(),
		field.Int64("user_id").Optional().Nillable(), // 鉴权 key 归属用户（context 传递，0 = 无）
		field.Int64("key_id").Optional().Nillable(),  // 鉴权 key（context 传递，0 = 无）
		field.String("model").Default(""),
		field.String("mapped_model").Optional().Nillable(),
		field.Enum("format").
			Values("openai-chat", "openai-responses", "openai-responses-ws", "anthropic"),
		// 用户裁决（err_logs 分表设计 + 升级原则）：usage_logs 瘦身去 2 留 1——
		// status_code（成功行恒 200 无信息量）与 error_message（纯排障文本列）
		// 移入 err_logs 独立审计表；error_type 保留（半异常计费行标记；分表后
		// usage_logs = 纯计费明细（仅 cost>0），值域收敛为 none/abort 两值——
		// 4xx/5xx/network 等 cost=0 错误行不写 usage_logs（不计费不入明细）。
		// 错误审计字段（status_code/error_message 等）由 err_logs 承载（见
		// errlog.go）。
		field.String("error_type").Default("none"),
		field.Int64("latency_ms").Default(0),
		// TTFT（首 token 时间毫秒）：caller 流式首 chunk 采集（ctx 传递）；
		// 非流式/失败/无首 token 路径 = NULL。
		field.Int64("ttft_ms").Optional().Nillable(),
		field.Int64("input_tokens").Default(0),
		// 价格快照（每 M token 毫分，1 USD = 100,000 毫分；pricing 同款单位）：
		// 请求时点生效基础单价，applyBilling 填充（GetPrice 已取价，零额外查找）；
		// 各价格列紧邻其 tokens 列（单价 × tokens = cost 可读性）。null =
		// 未计费路径（no_price 防御）；缓存价 null = 该请求无缓存读或无缓存价。
		field.Int64("price_input_millis").Optional().Nillable(),
		field.Int64("output_tokens").Default(0),
		field.Int64("price_output_millis").Optional().Nillable(),
		field.Int64("total_tokens").Default(0),
		field.Int64("cache_read_tokens").Default(0),
		field.Int64("price_cache_read_millis").Optional().Nillable(),
		field.Int64("cache_creation_tokens").Default(0),
		field.Int64("price_cache_creation_millis").Optional().Nillable(),
		// 计费列（Phase 5）：cost 毫分（1 USD = 100,000 毫分）；billing_tier
		// 请求 service_tier 归一化值（priority/flex/fast/auto；nil = 未计费路径）；
		// above_hit 任一分量超 above_threshold 命中分段；overdraft 本次扣费透支
		// （负余额）。错误请求（402/4xx）cost = 0。
		field.Int64("cost").Default(0),
		field.String("billing_tier").Optional().Nillable(),
		field.Bool("above_hit").Default(false),
		field.Bool("overdraft").Default(false),
		field.Time("created_at").Default(time.Now),
	}
}

func (UsageLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at"),
		index.Fields("group_id", "created_at"),
		index.Fields("account_id", "created_at"),
		index.Fields("user_id", "created_at"),
		index.Fields("key_id", "created_at"),
	}
}
