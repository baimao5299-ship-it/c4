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

	"go-proxy-mini/internal/ent/usagelog"
)

// PartitionRepo 管理 usagelog 按日分区（Phase 5 T4.5，用户决策 2026-08-09）：
// 50k 并发量级 usagelog 增长 ~4.3 亿行/天——逐行 DELETE 清理不可行，按月分区
// 单分区 26 亿行仍不可行 → PostgreSQL 原生分区表 PARTITION BY RANGE (created_at)，
// 每日一区（~8600 万行），保留期满直接 DROP TABLE O(1)（30 天 = 30 分区，
// 查询剪枝命中 1~30）。
//
// 主键 (id, created_at)（分区表硬约束：主键必须含分区键）；id 由专用序列
// usage_logs_id_seq 生成（ent 生成的 INSERT 不带 id 列 → 走 DEFAULT nextval，
// 与普通表 bigserial 语义一致，插入自动路由到 created_at 所在分区）。序列
// OWNED BY 表列 → DROP TABLE 级联回收。
type PartitionRepo struct {
	// driver 为 raw SQL 入口（ent 无分区 DDL 能力；bootstrap/retention 均走
	// dialect.Driver.Exec/Query —— execUpdate 同构，txDriver 下同连接）。
	driver dialect.Driver
}

// 分区名 = usagelog_YYYYMMDD（UTC 日；分区边界按 UTC 零点对齐，避免会话
// TimeZone 干扰——分区名即日期，retention 无需查元数据）。
func usageLogPartitionName(d time.Time) string {
	return "usage_logs_" + d.UTC().Format("20060102")
}

var usageLogPartitionRe = regexp.MustCompile(`^usage_logs_(\d{8})$`)

// usageLogPartitionDate 从分区名解析日期（retention DROP 边界判定）。
func usageLogPartitionDate(name string) (time.Time, bool) {
	m := usageLogPartitionRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102", m[1])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// usageLogColumnDefs 分区表列定义（单一事实源，评审 I-1 双向锚）：静态建表
// DDL 与幂等补列 ALTER 均由本列表生成——列集合双向相等，任一侧被绕过/手改
// 立即被 TestUsageLogAlignColumnsMatchCreateDDL 捕获（防"向静态 DDL 加列忘加
// align"这一 P1 同型复发，含类型漂移——列定义字符串整体生成）。列定义与 ent
// schema 完全一致，仅主键与 id 生成方式不同（见 usageLogCreateDDL 注释）。
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
	`status_code bigint NOT NULL DEFAULT 0`,
	`error_type varchar NOT NULL DEFAULT 'none'`,
	`error_message varchar NULL`,
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

// usageLogCreateDDL 分区表 DDL（由 usageLogColumnDefs 生成；列定义与 ent
// schema 完全一致，仅主键与 id 生成方式不同——ent migrate 主键 (id) 在分区表
// 上不可行，见 EnsureUsageLogPartitioned 注释与 usageLogMigrateHook；id 走
// 序列 nextval，语义同 ent bigserial）。
var usageLogCreateDDL = "CREATE TABLE usage_logs (\n\t" +
	strings.Join(usageLogColumnDefs, ",\n\t") + ",\n\t" +
	"PRIMARY KEY (id, created_at)\n" +
	") PARTITION BY RANGE (created_at)"

// usageLogIndexDDLs 对齐 ent schema Indexes（同名同列；分区表父表索引为
// 分区索引，子分区自动继承）。
var usageLogIndexDDLs = []string{
	`CREATE INDEX usagelog_created_at ON usage_logs (created_at)`,
	`CREATE INDEX usagelog_group_id_created_at ON usage_logs (group_id, created_at)`,
	`CREATE INDEX usagelog_account_id_created_at ON usage_logs (account_id, created_at)`,
	`CREATE INDEX usagelog_user_id_created_at ON usage_logs (user_id, created_at)`,
	`CREATE INDEX usagelog_key_id_created_at ON usage_logs (key_id, created_at)`,
}

// usageLogAlignColumnDDLs 存量分区表幂等补列 ALTER（由 usageLogColumnDefs
// 生成——全列 ADD COLUMN IF NOT EXISTS；已存在列 no-op，缺失列补齐）。P1
// （压测 2026-08-11 修复）：price 快照列/ttft_ms 合入后创建的旧分区表缺列——
// ent migrate 经钩子跳过 usagelog（分区表 diff 规划期必失败）、bootstrap 幂等
// （已分区 → 仅补分区）从不 ALTER 补列 → 新二进制连旧库 billing INSERT 即
// 42703 列不存在、usage 计费路径全停。PG12+ 父表 ALTER 自动传播到全部分区
// （无需逐分区 ALTER）。
var usageLogAlignColumnDDLs = func() []string {
	ddls := make([]string, 0, len(usageLogColumnDefs))
	for _, col := range usageLogColumnDefs {
		ddls = append(ddls, "ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS "+col)
	}
	return ddls
}()

// IsUsageLogPartitioned 查 pg_partitioned_table（pg_class.relkind='p'）判断
// usagelog 是否已是分区表（bootstrap 幂等判定）。
func (r *PartitionRepo) IsUsageLogPartitioned(ctx context.Context) (bool, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = current_schema() AND c.relname = 'usage_logs' AND c.relkind = 'p'`, []any{}, rows); err != nil {
		return false, err
	}
	defer rows.Close()
	n, err := entsql.ScanInt(rows)
	if err != nil {
		return false, err
	}
	return n > 0, nil
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
// EnsureUsageLogPartitioned），其余错误原样返回。
func (r *PartitionRepo) execDDLTolerateDup(ctx context.Context, query string) error {
	if err := r.execDDL(ctx, query); err != nil {
		if isDuplicateObject(err) {
			return nil
		}
		return err
	}
	return nil
}

// alignUsageLogColumns 幂等补列（ADD COLUMN IF NOT EXISTS）：存量分区表路径
// 按静态 DDL 全列对齐（缺失列补齐——P1 修复；新建/已有列 no-op；父表 ALTER
// 自动传播全部分区，PG12+）。多实例并发安全：ADD COLUMN 在父表上
// AccessExclusive 锁串行，IF NOT EXISTS 幂等收敛。
func (r *PartitionRepo) alignUsageLogColumns(ctx context.Context) error {
	for _, ddl := range usageLogAlignColumnDDLs {
		if err := r.execDDL(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}

// EnsureUsageLogPartitioned bootstrap（幂等，main 装配在 ent migrate 之后调用）：
// usagelog 已是分区表 → 仅确保 当日→明日 分区存在后返回；未分区（含 ent
// migrate 之前按旧 schema 建的普通表）→ DROP 重建分区表 + 序列 + 索引 + 预建
// 分区。该删删语义（用户决策 2026-08-09）：不向后兼容，存量普通表数据直接
// 丢弃（DB 可重建；usagelog 为明细流水，无外键引用）。
//
// 多实例语义（评审 I-1）：两实例同时启动/升级时，"是否已分区"判定与 CREATE
// 之间另一实例可能已建对象——所有 CREATE 步骤（分区表/索引/日分区）对 42P07
// （duplicate_object）容忍后继续，双方幂等收敛。DROP 为 IF EXISTS 不报错；
// 理论窗口下并发实例的 DROP 误删对方刚建的分区表时，双方同样经 42P07 容忍
// 重建索引/分区，最终状态一致（对象集合收敛）。
func (r *PartitionRepo) EnsureUsageLogPartitioned(ctx context.Context, now time.Time) error {
	parted, err := r.IsUsageLogPartitioned(ctx)
	if err != nil {
		return err
	}
	if !parted {
		if err := r.execDDL(ctx, `DROP TABLE IF EXISTS usage_logs`); err != nil {
			return fmt.Errorf("drop plain usage_logs: %w", err)
		}
		// 序列独立于表创建（CREATE TABLE 的 DEFAULT 需先存在）；OWNED BY 使
		// DROP TABLE 级联回收（serial 同款生命周期）。IF NOT EXISTS 并发下
		// 仍可能 23505（catalog 唯一索引竞态，实测），同样容忍。
		if err := r.execDDLTolerateDup(ctx, `CREATE SEQUENCE IF NOT EXISTS usage_logs_id_seq`); err != nil {
			return fmt.Errorf("create usage_logs_id_seq: %w", err)
		}
		if err := r.execDDLTolerateDup(ctx, usageLogCreateDDL); err != nil {
			return fmt.Errorf("create partitioned usage_logs: %w", err)
		}
		if err := r.execDDL(ctx, `ALTER SEQUENCE usage_logs_id_seq OWNED BY usage_logs.id`); err != nil {
			return fmt.Errorf("own sequence: %w", err)
		}
		for _, idx := range usageLogIndexDDLs {
			if err := r.execDDLTolerateDup(ctx, idx); err != nil {
				return fmt.Errorf("create usagelog index: %w", err)
			}
		}
	}
	// P1（压测 2026-08-11）：存量分区表 schema 对齐——旧库已建分区表时 bootstrap
	// 从不 ALTER 补列（幂等仅补分区）→ 新二进制连旧库 billing INSERT 42703
	// 全量失败。幂等 ADD COLUMN IF NOT EXISTS 按静态 DDL 全列对齐（新建路径
	// 全 no-op）。
	if err := r.alignUsageLogColumns(ctx); err != nil {
		return fmt.Errorf("align usagelog columns: %w", err)
	}
	return r.EnsureUsageLogPartitions(ctx, now, now.AddDate(0, 0, 1))
}

// EnsureUsageLogPartitions 确保 [trunc(now), trunc(until)] 每日分区存在
// （幂等：已存在跳过；until 早于 now → 仅 now 当日）。bootstrap 与 retention
// worker 共用——防日界竞态：分区未建时插入跨日 row 会整体失败（PG 对分区表
// 无自动建分区），必须预留未来分区。start/end 边界统一由调用方传入的 now
// 推导（评审 I-2：不内部取 time.Now()，测试可注入任意时钟；worker 每轮
// 现取 now 传入）。
func (r *PartitionRepo) EnsureUsageLogPartitions(ctx context.Context, now, until time.Time) error {
	start := now.UTC().Truncate(24 * time.Hour)
	end := until.UTC().Truncate(24 * time.Hour)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		name := usageLogPartitionName(d)
		ok, err := r.partitionExists(ctx, name)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		from := d.Format("2006-01-02 15:04:05-07")
		to := d.AddDate(0, 0, 1).Format("2006-01-02 15:04:05-07")
		if err := r.execDDL(ctx, fmt.Sprintf(`CREATE TABLE %s PARTITION OF usage_logs FOR VALUES FROM ('%s') TO ('%s')`, name, from, to)); err != nil {
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

// DropUsageLogPartitionsBefore DROP 分区下界日期早于 cutoff 的分区（O(1)，
// 按分区名日期判定，无需查元数据）；返回删除个数。保留 >= cutoff 的分区。
func (r *PartitionRepo) DropUsageLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid JOIN pg_class p ON p.oid = i.inhparent JOIN pg_namespace n ON n.oid = c.relnamespace WHERE p.relname = 'usage_logs' AND n.nspname = current_schema()`, []any{}, rows); err != nil {
		return 0, err
	}
	names := []string{}
	if err := entsql.ScanSlice(rows, &names); err != nil {
		return 0, err
	}
	cut := cutoff.UTC().Truncate(24 * time.Hour)
	dropped := 0
	for _, name := range names {
		d, ok := usageLogPartitionDate(name)
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

// usageLogMigrateHook 让 ent migrate 跳过 usagelog 表——分区表 DDL 由
// EnsureUsageLogPartitioned 独占管理。真实 PG 实测结论（2026-08-09，
// ent v0.14.6 + atlas v0.36.2 + PostgreSQL 18）：atlas 能识别已存在的分区表
// （分区键属性），与 ent schema 的普通表定义 diff 时在规划期直接报错
// "sql/schema: partition key cannot be dropped from \"usage_logs\""——
// 即任何"普通表 → 分区表"的 diff 都不可行，ent migrate 对分区表必然失败，
// 且无 migrate 选项可容忍（ent 无禁用主键/分区键 diff 的选项）。故用
// schema.WithHooks 在 Create 前过滤该表，ent 永不 diff/DDL usagelog；
// 该表的存在性/列结构/索引完全由 bootstrap 维护（与 ent schema 列定义
// 一致，见 usageLogCreateDDL）。
func usageLogMigrateHook() schema.MigrateOption {
	return schema.WithHooks(func(next schema.Creator) schema.Creator {
		return schema.CreateFunc(func(ctx context.Context, tables ...*schema.Table) error {
			kept := make([]*schema.Table, 0, len(tables))
			for _, t := range tables {
				if t.Name != usagelog.Table {
					kept = append(kept, t)
				}
			}
			return next.Create(ctx, kept...)
		})
	})
}
