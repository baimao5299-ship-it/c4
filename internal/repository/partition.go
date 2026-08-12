// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is7qin/c3api/internal/ent/errlog"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/ent/usagestat"
)

// PartitionRepo 管理按日分区表（Phase 5 T4.5，用户决策 2026-08-09；err_logs/
// usage_stats 分区复用同路线，用户决策 2026-08-11）：50k 并发量级 usage_logs
// 增长 ~4.3 亿行/天——逐行 DELETE 清理不可行，按月分区单分区 26 亿行仍不可行
// → PostgreSQL 原生分区表 PARTITION BY RANGE (分区键)，每日一区（usage_logs
// ~8600 万行），保留期满直接 DROP TABLE O(1)。三表：
//   - usage_logs 计费明细（分区键 created_at；默认 30 天保留）
//   - err_logs 错误审计明细（分区键 created_at；风暴背压采样后量可控，默认
//     7 天短保留，见 usage.ErrLogRetentionDays）
//   - usage_stats 统计聚合（分区键 bucket_time——小时桶聚合 24 桶/日分区；
//     默认 180 天保留；用户裁决：PG DELETE 不释放空间，清理必须分区 DROP）
//
// 主键 (id, 分区键)（分区表硬约束：主键必须含分区键）；id 由专用序列
// {table}_id_seq 生成（ent 生成的 INSERT 不带 id 列 → 走 DEFAULT nextval，
// 与普通表 bigserial 语义一致，插入自动路由到分区键所在分区）。序列 OWNED BY
// 表列 → DROP TABLE 级联回收。
//
// Ensure/Drop 均按表参数化（表名 + 分区键 + 日期）——三表共用一套实现（单一
// 实现防漂移，P1 教训同款）；幂等语义全表通用（IF NOT EXISTS / IF EXISTS、
// 42P07/duplicate_object 容忍、并发实例安全）。
type PartitionRepo struct {
	// driver 为 raw SQL 入口（ent 无分区 DDL 能力；bootstrap/retention 均走
	// dialect.Driver.Exec/Query —— execUpdate 同构，txDriver 下同连接）。
	driver dialect.Driver
}

// 分区名 = {table}_{YYYYMMDD}（UTC 日；分区边界按 UTC 零点对齐，避免会话
// TimeZone 干扰——分区名即日期，retention 无需查元数据）。
func tablePartitionName(table string, d time.Time) string {
	return table + "_" + d.UTC().Format("20060102")
}

// tablePartitionRe 从分区名解析日期（retention DROP 边界判定）。
func tablePartitionRe(table string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(table) + `_(\d{8})$`)
}

func tablePartitionDate(table, name string) (time.Time, bool) {
	m := tablePartitionRe(table).FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102", m[1])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// partitionedCreateDDL 分区表 DDL（由列定义事实源生成；列定义与 ent schema
// 完全一致，仅主键与 id 生成方式不同——ent migrate 主键 (id) 在分区表上不可行，
// 见 ensureTablePartitioned 注释与 migrateHookExcludesPartitioned；id 走序列
// nextval，语义同 ent bigserial）。
// partitionCol 为分区键列（usage_logs/err_logs = created_at；usage_stats =
// bucket_time——用户裁决 2026-08-11：三表统一分区机制，主键必含分区键）。
func partitionedCreateDDL(table, partitionCol string, columnDefs []string) string {
	return "CREATE TABLE " + table + " (\n\t" +
		strings.Join(columnDefs, ",\n\t") + ",\n\t" +
		"PRIMARY KEY (id, " + partitionCol + ")\n" +
		") PARTITION BY RANGE (" + partitionCol + ")"
}

// alignColumnDDLs 存量分区表幂等补列 ALTER（由列定义事实源生成——全列 ADD
// COLUMN IF NOT EXISTS；已存在列 no-op，缺失列补齐）。P1（压测 2026-08-11
// 修复）：price 快照列/ttft_ms 合入后创建的旧分区表缺列——ent migrate 经钩子
// 跳过分区表（diff 规划期必失败）、bootstrap 幂等（已分区 → 仅补分区）从不
// ALTER 补列 → 新二进制连旧库 INSERT 即 42703 列不存在、写路径全停。PG12+
// 父表 ALTER 自动传播到全部分区（无需逐分区 ALTER）。
func alignColumnDDLs(table string, columnDefs []string) []string {
	ddls := make([]string, 0, len(columnDefs))
	for _, col := range columnDefs {
		ddls = append(ddls, "ALTER TABLE "+table+" ADD COLUMN IF NOT EXISTS "+col)
	}
	return ddls
}

// usageLogColumnDefs usage_logs 分区表列定义（单一事实源，评审 I-1 双向锚）：
// 静态建表 DDL 与幂等补列 ALTER 均由本列表生成——列集合双向相等，任一侧被绕过/
// 手改立即被 TestUsageLogAlignColumnsMatchCreateDDL 捕获（防"向静态 DDL 加列忘加
// align"这一 P1 同型复发，含类型漂移——列定义字符串整体生成）。列定义与 ent
// schema 完全一致，仅主键与 id 生成方式不同（见 partitionedCreateDDL 注释）。
// 用户裁决（err_logs 分表）：usage_logs 瘦身去 2 留 1——status_code 与
// error_message 列移除（错误审计由 err_logs 承载，见 errLogColumnDefs）；残留
// 列存量库不 DROP（bootstrap 只加不减幂等），新库重建生效。
var usageLogColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('usage_logs_id_seq'::regclass)`,
	`request_id varchar NOT NULL`,
	`group_id bigint NULL`,
	`account_id bigint NULL`,
	`template_id bigint NULL`,
	`user_id bigint NULL`,
	`key_id bigint NULL`,
	`model varchar NOT NULL DEFAULT ''`,
	`mapped_model varchar NULL`,
	`format varchar NOT NULL`,
	`error_type varchar NOT NULL DEFAULT 'none'`,
	`latency_ms bigint NOT NULL DEFAULT 0`,
	`ttft_ms bigint NULL`,
	`input_tokens bigint NOT NULL DEFAULT 0`,
	`price_input_millis bigint NULL`,
	`output_tokens bigint NOT NULL DEFAULT 0`,
	`price_output_millis bigint NULL`,
	`total_tokens bigint NOT NULL DEFAULT 0`,
	`cache_read_tokens bigint NOT NULL DEFAULT 0`,
	`price_cache_read_millis bigint NULL`,
	`cache_creation_tokens bigint NOT NULL DEFAULT 0`,
	`price_cache_creation_millis bigint NULL`,
	`cost bigint NOT NULL DEFAULT 0`,
	`billing_tier varchar NULL`,
	`above_hit boolean NOT NULL DEFAULT false`,
	`overdraft boolean NOT NULL DEFAULT false`,
	`created_at timestamptz NOT NULL`,
}

var usageLogCreateDDL = partitionedCreateDDL("usage_logs", "created_at", usageLogColumnDefs)

// usageLogIndexDDLs 对齐 ent schema Indexes（同名同列；分区表父表索引为
// 分区索引，子分区自动继承）。
var usageLogIndexDDLs = []string{
	`CREATE INDEX usagelog_created_at ON usage_logs (created_at)`,
	`CREATE INDEX usagelog_group_id_created_at ON usage_logs (group_id, created_at)`,
	`CREATE INDEX usagelog_account_id_created_at ON usage_logs (account_id, created_at)`,
	`CREATE INDEX usagelog_user_id_created_at ON usage_logs (user_id, created_at)`,
	`CREATE INDEX usagelog_key_id_created_at ON usage_logs (key_id, created_at)`,
}

var usageLogAlignColumnDDLs = alignColumnDDLs("usage_logs", usageLogColumnDefs)

// errLogColumnDefs err_logs 分区表列定义（单一事实源，与 ent schema 完全一致，
// 对齐锚 TestErrLogAlignColumnsMatchCreateDDL 双向断言）：错误审计瘦表——无
// token/价格列；status_code/error_message（usage_logs 瘦身去掉的排障列）+ 审计
// 归属（group/account/template/user/key）+ billing_tier（评审 I-3：tier reject
// 的 tier 维度审计保留）。
var errLogColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('err_logs_id_seq'::regclass)`,
	`request_id varchar NOT NULL`,
	`group_id bigint NULL`,
	`account_id bigint NULL`,
	`template_id bigint NULL`,
	`user_id bigint NULL`,
	`key_id bigint NULL`,
	`model varchar NOT NULL DEFAULT ''`,
	`format varchar NOT NULL`,
	`status_code bigint NOT NULL DEFAULT 0`,
	`error_type varchar NOT NULL DEFAULT 'none'`,
	`error_message varchar NULL`,
	`latency_ms bigint NOT NULL DEFAULT 0`,
	`billing_tier varchar NULL`,
	`created_at timestamptz NOT NULL`,
}

var errLogCreateDDL = partitionedCreateDDL("err_logs", "created_at", errLogColumnDefs)

// errLogIndexDDLs 对齐 ent schema Indexes（同名同列；分区表父表索引为分区
// 索引，子分区自动继承）：created_at 时间窗口查询/清理 + (group_id/user_id,
// created_at) 查询面（架构审查 S1——/err_logs 按用户/组过滤）。
var errLogIndexDDLs = []string{
	`CREATE INDEX errlog_created_at ON err_logs (created_at)`,
	`CREATE INDEX errlog_group_id_created_at ON err_logs (group_id, created_at)`,
	`CREATE INDEX errlog_user_id_created_at ON err_logs (user_id, created_at)`,
}

var errLogAlignColumnDDLs = alignColumnDDLs("err_logs", errLogColumnDefs)

// usageStatsColumnDefs usage_stats 分区表列定义（单一事实源；用户裁决
// 2026-08-11：usage_stats 与明细两表统一分区机制——PG DELETE 不释放空间，
// 180 天保留清理必须 DROP 分区 O(1)）。列定义与 ent schema 完全一致；分区键
// bucket_time（小时桶聚合 24 桶/区）。id 走 usage_stats_id_seq（ent bigserial
// 同款语义——Upsert 不带 id 列走 DEFAULT nextval；该表由 ent migrate 建过普通
// 表后 bootstrap DROP 重建分区表，不向后兼容，存量聚合丢弃可重建）。
var usageStatsColumnDefs = []string{
	`id bigint NOT NULL DEFAULT nextval('usage_stats_id_seq'::regclass)`,
	`bucket_time timestamptz NOT NULL`,
	`group_id bigint NOT NULL DEFAULT 0`,
	`account_id bigint NOT NULL DEFAULT 0`,
	`template_id bigint NOT NULL DEFAULT 0`,
	`user_id bigint NOT NULL DEFAULT 0`,
	`model varchar NOT NULL DEFAULT ''`,
	`is_error boolean NOT NULL DEFAULT false`,
	`request_count bigint NOT NULL DEFAULT 0`,
	`error_count bigint NOT NULL DEFAULT 0`,
	`input_tokens bigint NOT NULL DEFAULT 0`,
	`output_tokens bigint NOT NULL DEFAULT 0`,
	`total_tokens bigint NOT NULL DEFAULT 0`,
	`cache_read_tokens bigint NOT NULL DEFAULT 0`,
	`cache_creation_tokens bigint NOT NULL DEFAULT 0`,
	`cost bigint NOT NULL DEFAULT 0`,
	`total_latency_ms bigint NOT NULL DEFAULT 0`,
	`updated_at timestamptz NOT NULL`,
}

var usageStatsCreateDDL = partitionedCreateDDL("usage_stats", "bucket_time", usageStatsColumnDefs)

// usageStatsIndexDDLs 对齐 ent schema Indexes（同名同列；分区表父表索引为分区
// 索引，子分区自动继承）。唯一索引含分区键 bucket_time（分区表硬约束：唯一
// 索引必须含分区键；顺带即 Upsert 的 ON CONFLICT 目标列序——batched COPY
// 两阶段 merge 在分区表上行为不变）。
var usageStatsIndexDDLs = []string{
	`CREATE INDEX usagestat_bucket_time ON usage_stats (bucket_time)`,
	`CREATE UNIQUE INDEX usagestat_bucket_time_group_id_account_id_template_id_user_id_model_is_error ON usage_stats (bucket_time, group_id, account_id, template_id, user_id, model, is_error)`,
}

var usageStatsAlignColumnDDLs = alignColumnDDLs("usage_stats", usageStatsColumnDefs)

// IsTablePartitioned 查 pg_partitioned_table（pg_class.relkind='p'）判断指定
// 表是否已是分区表（bootstrap 幂等判定）。
func (r *PartitionRepo) IsTablePartitioned(ctx context.Context, table string) (bool, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = current_schema() AND c.relname = $1 AND c.relkind = 'p'`, []any{table}, rows); err != nil {
		return false, err
	}
	defer rows.Close()
	n, err := entsql.ScanInt(rows)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// IsUsageLogPartitioned usage_logs 是否分区表（bootstrap 幂等判定；既有 API）。
func (r *PartitionRepo) IsUsageLogPartitioned(ctx context.Context) (bool, error) {
	return r.IsTablePartitioned(ctx, "usage_logs")
}

// IsErrLogPartitioned err_logs 是否分区表（bootstrap 幂等判定）。
func (r *PartitionRepo) IsErrLogPartitioned(ctx context.Context) (bool, error) {
	return r.IsTablePartitioned(ctx, "err_logs")
}

// IsUsageStatsPartitioned usage_stats 是否分区表（bootstrap 幂等判定）。
func (r *PartitionRepo) IsUsageStatsPartitioned(ctx context.Context) (bool, error) {
	return r.IsTablePartitioned(ctx, "usage_stats")
}

// partitionExists 分区表是否存在（幂等预建判定；pgx 下 to_regclass 错误处理
// 复杂，直接查 pg_class，任意 relkind 均可——分区子表 relkind='r'）。
func (r *PartitionRepo) partitionExists(ctx context.Context, name string) (bool, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = current_schema() AND c.relname = $1`, []any{name}, rows); err != nil {
		return false, err
	}
	defer rows.Close()
	n, err := entsql.ScanInt(rows)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// execDDL 执行无参数 DDL（bootstrap/retention；v 形参 *sql.Result 与
// execUpdate 同构）。
func (r *PartitionRepo) execDDL(ctx context.Context, query string) error {
	var res sql.Result
	return r.driver.Exec(ctx, query, []any{}, &res)
}

// isDuplicateObject 判断"对象已存在"类竞态错误（多实例并发 bootstrap/预建
// 分区撞名容忍；ent Conn.Exec 原样透传 pgconn 错误，无需解包）：
//   - 42P07 duplicate_object：无 IF NOT EXISTS 的 CREATE（分区表/索引/日分区）
//     撞已存在对象；
//   - 23505 unique_violation：IF NOT EXISTS 的 CREATE SEQUENCE 并发创建时
//     "检查-插入"在 pg_class 唯一索引上竞态（PG 已知行为，实测出现）——
//     仅在 bootstrap 的 CREATE 步骤使用本判定，23505 只能来自并发建对象。
func isDuplicateObject(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "42P07" || pgErr.Code == "23505"
}

// execDDLTolerateDup 执行 DDL；对象已存在类错误（42P07/23505，见
// isDuplicateObject）视为成功（并发实例已建，多实例语义见
// ensureTablePartitioned），其余错误原样返回。
func (r *PartitionRepo) execDDLTolerateDup(ctx context.Context, query string) error {
	if err := r.execDDL(ctx, query); err != nil {
		if isDuplicateObject(err) {
			return nil
		}
		return err
	}
	return nil
}

// alignTableColumns 幂等补列（ADD COLUMN IF NOT EXISTS）：存量分区表路径按
// 静态 DDL 全列对齐（缺失列补齐——P1 修复；新建/已有列 no-op；父表 ALTER
// 自动传播全部分区，PG12+）。多实例并发安全：ADD COLUMN 在父表上
// AccessExclusive 锁串行，IF NOT EXISTS 幂等收敛。
func (r *PartitionRepo) alignTableColumns(ctx context.Context, table string, alignDDLs []string) error {
	for _, ddl := range alignDDLs {
		if err := r.execDDL(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}

// ensureTablePartitioned bootstrap（幂等，main 装配在 ent migrate 之后调用）：
// 指定表已是分区表 → 仅确保 当日→明日 分区存在后返回；未分区（含 ent migrate
// 之前按旧 schema 建的普通表）→ DROP 重建分区表 + 序列 + 索引 + 预建分区。
// 该删删语义（用户决策 2026-08-09）：不向后兼容，存量普通表数据直接丢弃
// （DB 可重建；明细流水，无外键引用）。
//
// 多实例语义（评审 I-1）：两实例同时启动/升级时，"是否已分区"判定与 CREATE
// 之间另一实例可能已建对象——所有 CREATE 步骤（分区表/索引/日分区）对 42P07
// （duplicate_object）容忍后继续，双方幂等收敛。DROP 为 IF EXISTS 不报错；
// 理论窗口下并发实例的 DROP 误删对方刚建的分区表时，双方同样经 42P07 容忍
// 重建索引/分区，最终状态一致（对象集合收敛）。
func (r *PartitionRepo) ensureTablePartitioned(ctx context.Context, table, partitionCol string, columnDefs, indexDDLs, alignDDLs []string, now time.Time) error {
	parted, err := r.IsTablePartitioned(ctx, table)
	if err != nil {
		return err
	}
	if !parted {
		if err := r.execDDL(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			return fmt.Errorf("drop plain %s: %w", table, err)
		}
		// 序列独立于表创建（CREATE TABLE 的 DEFAULT 需先存在）；OWNED BY 使
		// DROP TABLE 级联回收（serial 同款生命周期）。IF NOT EXISTS 并发下
		// 仍可能 23505（catalog 唯一索引竞态，实测），同样容忍。
		seqName := table + "_id_seq"
		if err := r.execDDLTolerateDup(ctx, `CREATE SEQUENCE IF NOT EXISTS `+seqName); err != nil {
			return fmt.Errorf("create %s: %w", seqName, err)
		}
		if err := r.execDDLTolerateDup(ctx, partitionedCreateDDL(table, partitionCol, columnDefs)); err != nil {
			return fmt.Errorf("create partitioned %s: %w", table, err)
		}
		if err := r.execDDL(ctx, `ALTER SEQUENCE `+seqName+` OWNED BY `+table+`.id`); err != nil {
			return fmt.Errorf("own sequence: %w", err)
		}
		for _, idx := range indexDDLs {
			if err := r.execDDLTolerateDup(ctx, idx); err != nil {
				return fmt.Errorf("create %s index: %w", table, err)
			}
		}
	}
	// P1（压测 2026-08-11）：存量分区表 schema 对齐——旧库已建分区表时 bootstrap
	// 从不 ALTER 补列（幂等仅补分区）→ 新二进制连旧库 INSERT 42703 全量失败。
	// 幂等 ADD COLUMN IF NOT EXISTS 按静态 DDL 全列对齐（新建路径全 no-op）。
	if err := r.alignTableColumns(ctx, table, alignDDLs); err != nil {
		return fmt.Errorf("align %s columns: %w", table, err)
	}
	return r.EnsureTablePartitions(ctx, table, now, now.AddDate(0, 0, 1))
}

// EnsureUsageLogPartitioned usage_logs 分区 bootstrap（既有 API，见
// ensureTablePartitioned；分区键 created_at）。
func (r *PartitionRepo) EnsureUsageLogPartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "usage_logs", "created_at", usageLogColumnDefs, usageLogIndexDDLs, usageLogAlignColumnDDLs, now)
}

// EnsureErrLogPartitioned err_logs 分区 bootstrap（同路线复用：独立列事实源 +
// 独立序列 err_logs_id_seq + 独立保留期；分区键 created_at）。
func (r *PartitionRepo) EnsureErrLogPartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "err_logs", "created_at", errLogColumnDefs, errLogIndexDDLs, errLogAlignColumnDDLs, now)
}

// EnsureUsageStatsPartitioned usage_stats 分区 bootstrap（用户裁决 2026-08-11：
// 三表统一分区机制；分区键 bucket_time——小时桶聚合 24 桶/日分区）。迁移语义
// 与明细两表一致（该删删：存量普通表 DROP 重建分区表，聚合可重建）。
func (r *PartitionRepo) EnsureUsageStatsPartitioned(ctx context.Context, now time.Time) error {
	return r.ensureTablePartitioned(ctx, "usage_stats", "bucket_time", usageStatsColumnDefs, usageStatsIndexDDLs, usageStatsAlignColumnDDLs, now)
}

// EnsureTablePartitions 确保 [trunc(now), trunc(until)] 每日分区存在
// （幂等：已存在跳过；until 早于 now → 仅 now 当日）。bootstrap 与 retention
// worker 共用——防日界竞态：分区未建时插入跨日 row 会整体失败（PG 对分区表
// 无自动建分区），必须预留未来分区。start/end 边界统一由调用方传入的 now
// 推导（评审 I-2：不内部取 time.Now()，测试可注入任意时钟；worker 每轮
// 现取 now 传入）。
func (r *PartitionRepo) EnsureTablePartitions(ctx context.Context, table string, now, until time.Time) error {
	start := now.UTC().Truncate(24 * time.Hour)
	end := until.UTC().Truncate(24 * time.Hour)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		name := tablePartitionName(table, d)
		ok, err := r.partitionExists(ctx, name)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		from := d.Format("2006-01-02 15:04:05-07")
		to := d.AddDate(0, 0, 1).Format("2006-01-02 15:04:05-07")
		if err := r.execDDL(ctx, fmt.Sprintf(`CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`, name, table, from, to)); err != nil {
			if isDuplicateObject(err) {
				// 并发实例已建该分区：重查确认后继续（多实例语义同上）
				ok2, err2 := r.partitionExists(ctx, name)
				if err2 != nil {
					return err2
				}
				if ok2 {
					continue
				}
				return fmt.Errorf("create partition %s: %w", name, err)
			}
			return fmt.Errorf("create partition %s: %w", name, err)
		}
	}
	return nil
}

// EnsureUsageLogPartitions usage_logs 每日分区预建（既有 API）。
func (r *PartitionRepo) EnsureUsageLogPartitions(ctx context.Context, now, until time.Time) error {
	return r.EnsureTablePartitions(ctx, "usage_logs", now, until)
}

// EnsureErrLogPartitions err_logs 每日分区预建（retention worker 共用）。
func (r *PartitionRepo) EnsureErrLogPartitions(ctx context.Context, now, until time.Time) error {
	return r.EnsureTablePartitions(ctx, "err_logs", now, until)
}

// EnsureUsageStatsPartitions usage_stats 每日分区预建（retention worker 共用；
// 分区键 bucket_time——日界分区从 now 当日零点起）。
func (r *PartitionRepo) EnsureUsageStatsPartitions(ctx context.Context, now, until time.Time) error {
	return r.EnsureTablePartitions(ctx, "usage_stats", now, until)
}

// DropTablePartitionsBefore DROP 指定表分区下界日期早于 cutoff 的分区（O(1)，
// 按分区名日期判定，无需查元数据）；返回删除个数。保留 >= cutoff 的分区。
func (r *PartitionRepo) DropTablePartitionsBefore(ctx context.Context, table string, cutoff time.Time) (int, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid JOIN pg_class p ON p.oid = i.inhparent JOIN pg_namespace n ON n.oid = c.relnamespace WHERE p.relname = $1 AND n.nspname = current_schema()`, []any{table}, rows); err != nil {
		return 0, err
	}
	names := []string{}
	if err := entsql.ScanSlice(rows, &names); err != nil {
		return 0, err
	}
	cut := cutoff.UTC().Truncate(24 * time.Hour)
	dropped := 0
	for _, name := range names {
		d, ok := tablePartitionDate(table, name)
		if !ok {
			continue
		}
		if d.Before(cut) {
			if err := r.execDDL(ctx, `DROP TABLE IF EXISTS `+name); err != nil {
				return dropped, fmt.Errorf("drop partition %s: %w", name, err)
			}
			dropped++
		}
	}
	return dropped, nil
}

// DropUsageLogPartitionsBefore usage_logs DROP 分区下界早于 cutoff 的分区
// （既有 API；O(1)）。保留 >= cutoff 的分区。
func (r *PartitionRepo) DropUsageLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "usage_logs", cutoff)
}

// DropErrLogPartitionsBefore err_logs DROP 分区下界早于 cutoff 的分区（独立
// 保留期：cutoff 由 usage.ErrLogRetentionDays 推导，两表各自调度）。
func (r *PartitionRepo) DropErrLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "err_logs", cutoff)
}

// DropUsageStatsPartitionsBefore usage_stats DROP 分区下界早于 cutoff 的分区
// （用户裁决 2026-08-11：PG DELETE 不释放空间，180 天保留清理必须分区 DROP
// O(1)——替代逐行 DELETE 方案；cutoff 由 usage.StatsRetentionDays 推导）。
func (r *PartitionRepo) DropUsageStatsPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.DropTablePartitionsBefore(ctx, "usage_stats", cutoff)
}

// migrateHookExcludesPartitioned 让 ent migrate 跳过分区表（usage_logs +
// err_logs + usage_stats）——分区表 DDL 由 ensureTablePartitioned 独占管理。
// 真实 PG 实测
// 结论（2026-08-09，ent v0.14.6 + atlas v0.36.2 + PostgreSQL 18）：atlas 能
// 识别已存在的分区表（分区键属性），与 ent schema 的普通表定义 diff 时在规划
// 期直接报错 "sql/schema: partition key cannot be dropped from ..."——即任何
// "普通表 → 分区表"的 diff 都不可行，ent migrate 对分区表必然失败，且无
// migrate 选项可容忍（ent 无禁用主键/分区键 diff 的选项）。故用 schema.WithHooks
// 在 Create 前过滤这些表，ent 永不 diff/DDL 分区表；表的存在性/列结构/索引
// 完全由 bootstrap 维护（与 ent schema 列定义一致，见 partitionedCreateDDL）。
func migrateHookExcludesPartitioned() schema.MigrateOption {
	return schema.WithHooks(func(next schema.Creator) schema.Creator {
		return schema.CreateFunc(func(ctx context.Context, tables ...*schema.Table) error {
			kept := make([]*schema.Table, 0, len(tables))
			for _, t := range tables {
				if t.Name != usagelog.Table && t.Name != errlog.Table && t.Name != usagestat.Table {
					kept = append(kept, t)
				}
			}
			return next.Create(ctx, kept...)
		})
	})
}
