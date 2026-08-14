package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type UsageStat struct{ ent.Schema }

func (UsageStat) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Time("bucket_time"),
		field.Int64("group_id").Default(0), // 0 = 无（唯一索引需要非 NULL）
		field.Int64("account_id").Default(0),
		field.Int64("template_id").Default(0),
		field.Int64("user_id").Default(0), // 0 = 无（鉴权失败/无 key）；/user/stats 按此过滤
		field.String("model").Default(""),
		field.Bool("is_error").Default(false),
		field.Int64("request_count").Default(0),
		field.Int64("error_count").Default(0),
		field.Int64("input_tokens").Default(0),
		field.Int64("output_tokens").Default(0),
		field.Int64("total_tokens").Default(0),
		field.Int64("cache_read_tokens").Default(0),
		field.Int64("cache_creation_tokens").Default(0),
		field.Int64("cost").Default(0), // 毫分（1 USD = 100,000 毫分）；计费预聚合，花费统计不扫明细
		// 按次调用（用户裁决 2026-08-14）：图片生成 = 张数、search = 1
		field.Int64("call_count").Default(0),
		// TTFT 三标量列（spec 2026-08-14）：avg 查询侧 Go 除（ttft_total/ttft_count）
		field.Int64("ttft_total_ms").Default(0),
		field.Int64("ttft_count").Default(0),
		field.Int64("ttft_max_ms").Default(0),
		// 注：ttft_hist bigint[10] 数组列 carve-out 不进 ent schema（ent 无 PG
		// 数组类型；field.Ints 等是 JSON 语义）——列定义唯一事实源在
		// repository/partition.go usageStatsColumnDefs，ScanStats 改 pgx 直查。
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (UsageStat) Indexes() []ent.Index {
	return []ent.Index{
		// /user/stats 查询形态：user_id EQ + bucket_time 范围（ScanStats）。
		// user_id 在复合唯一中居第 5 列，范围扫描逐行过滤低效，此前缀索引
		// 精确匹配（分区表侧 DDL 见 repository/partition.go usageStatsIndexDDLs）。
		index.Fields("user_id", "bucket_time"),
		index.Fields("bucket_time", "group_id", "account_id", "template_id", "user_id", "model", "is_error").Unique(),
	}
}
