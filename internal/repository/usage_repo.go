// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// usage_logs 明细查询/插入（消费面改名裁决：log_repo → usage 语义命名——/logs
// API 改名 /usages 后内部类型随改名，UsageRepo/UsageQuery/QueryUsages；错误审计
// 面由 errlog_repo.go（err_logs）承载）。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/usagelog"
)

type UsageQuery struct {
	GroupID   int64 // 0 = 不过滤
	AccountID int64
	UserID    int64 // 0 = 不过滤（/api/user/usages 强制 = 自己）
	KeyID     int64
	Model     string
	Format    string // 空 = 不过滤（无效值自然查空——与 model 同语义，契约不校验值域）
	ErrorType string // usage_logs = 纯计费明细（仅 cost>0）→ 值域收敛 none/abort（err_logs 分表后）
	From      *time.Time
	To        *time.Time
	Cursor    int64 // keyset 游标（上页最后一条 id；<=0 = 首页无 id 谓词）
	Limit     int
}

// UsageLogsSummary is the full-window financial aggregate for an admin usage
// log query. Cost estimates are nullable because historical or unpriced rows
// do not have a request-time upstream cost snapshot.
type UsageLogsSummary struct {
	RequestCount         int64
	CostedRequestCount   int64
	UserCharge           int64
	AttributedUserCharge int64
	UpstreamCost         *int64
	GrossProfit          *int64
	ProfitMarginBP       *int64
	LossRequestCount     int64
}

// ScanPublicChannelStats aggregates recent calls for a bounded set of public
// groups. usage_logs and err_logs intentionally overlap for aborts; the query
// de-duplicates non-empty request IDs before counting so the UI reports one
// call and one outcome per request.
func (r *UsageRepo) ScanPublicChannelStats(ctx context.Context, groupIDs []int64, from, to time.Time) (map[int64]*domain.PublicChannelStat, error) {
	if len(groupIDs) == 0 {
		return map[int64]*domain.PublicChannelStat{}, nil
	}
	if r.pool == nil {
		return nil, fmt.Errorf("usage repo: pgx pool not configured (repository.NewWithPG); cannot scan public channel stats")
	}
	rows, err := r.pool.Query(ctx, `WITH raw AS (
		SELECT group_id, request_id, latency_ms, created_at,
			(error_type <> 'none') AS failed, 'usage'::text AS source, id
		FROM usage_logs
		WHERE group_id = ANY($1) AND created_at >= $2 AND created_at < $3
		UNION ALL
		SELECT group_id, request_id, latency_ms, created_at,
			TRUE AS failed, 'error'::text AS source, id
		FROM err_logs
		WHERE group_id = ANY($1) AND created_at >= $2 AND created_at < $3
	), keyed AS (
		SELECT raw.*,
			CASE WHEN request_id IS NULL OR request_id = ''
				THEN source || ':' || id::text
				ELSE 'request:' || request_id END AS event_key
		FROM raw
	), dedup AS (
		SELECT DISTINCT ON (group_id, event_key)
			group_id, latency_ms, created_at, failed
		FROM keyed
		ORDER BY group_id, event_key, failed DESC, created_at DESC, id DESC
	)
	SELECT group_id,
		count(*)::bigint,
		count(*) FILTER (WHERE failed)::bigint,
		COALESCE(sum(GREATEST(latency_ms, 0)) FILTER (WHERE latency_ms > 0), 0)::bigint,
		count(*) FILTER (WHERE latency_ms > 0)::bigint,
		max(created_at)
	FROM dedup
	GROUP BY group_id`, groupIDs, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]*domain.PublicChannelStat, len(groupIDs))
	for rows.Next() {
		stat := &domain.PublicChannelStat{}
		if err := rows.Scan(&stat.GroupID, &stat.RequestCount, &stat.ErrorCount,
			&stat.LatencyTotalMS, &stat.LatencySampleCount, &stat.LastCalledAt); err != nil {
			return nil, err
		}
		out[stat.GroupID] = stat
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type UsageRepo struct {
	client *ent.Client
	// pool 为聚合 SQL 直查入口（ScanUsageAgg——usage_logs 含 raw_cost 等
	// SUM 聚合，ent 构建器无 SUM 能力，pgx 直查同 StatRepo carve-out 形态）；
	// NewWithPG 注入（生产与 ent driver 同 DSN），New 未注入 → 显式错误。
	pool *pgxpool.Pool
}

// usageAggMaxAccountIDs ScanUsageAgg 批量 ids 上限（= handler account_ids
// ≤100 契约——repo 层防御 handler 之外调用方，N5）。
const usageAggMaxAccountIDs = 100

func (r *UsageRepo) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	if len(logs) == 0 {
		return nil
	}
	builders := make([]*ent.UsageLogCreate, 0, len(logs))
	for _, l := range logs {
		builders = append(builders, buildUsageLogCreate(r.client, l))
	}
	_, err := r.client.UsageLog.CreateBulk(builders...).Save(ctx)
	return err
}

// buildUsageLogCreate 构建单条 usagelog 插入构建器（F2 单写点：usage flusher
// InsertBatch 是 usage_logs 唯一写者——计费游标消费面只翻 billed/overdraft，
// 不再插日志）。
// 计费列（Phase 5）：Cost 毫分（0 = 未计费/错误路径）；BillingTier 空 = 未计费
// 路径（落库 NULL）；AboveHit/Overdraft 布尔直接落。RawCost（spec 2026-08-18）
// 乘倍率前原始成本——恒落（对齐 SetCost，ent 缺省 0 无妨），COPY 路径
// usageLogRowValues 同序（两路径列集合锚定）。
// 时间/价格快照列（nil = NULL 落库，SQL 不写该列）：TTFTMS 首 token 时间毫秒
// （非流式/失败路径 nil）；Price*Millis 每 M token 毫分单价快照（未计费路径
// /无该分量 nil）。
// 统一计费模型功能调用分量（spec 2026-08-13）：CallCount 直接落（0 默认——
// 图片生成 = 张数、search = 1）；PricePerCallMillis **毫分/单元**（search 每次
// /图片每张，例外单位——per-call 计费不走 /1e6 除法，同原 price_per_image
// _millis 语义）——nil = NULL 落库。原图片 6 列已删：image token 并入
// InputTokens/OutputTokens（TotalTokens 口径不变）。
// 用户裁决（err_logs 分表）：StatusCode/ErrorMessage 为域内瞬态审计字段
// （err_logs 承载），不再写 usage_logs（该两列已从表移除——瘦身）。
// billed（F2 ledger-cursor，spec 2026-08-23）：出生标记直接透传（false=待对账
// 消费；true=关闭计费/匿名行出生吸收态），翻转只发生在对账事务内。
func buildUsageLogCreate(client *ent.Client, l *domain.UsageLog) *ent.UsageLogCreate {
	c := client.UsageLog.Create().
		SetRequestID(l.RequestID).
		SetModel(l.Model).
		SetFormat(usagelog.Format(l.Format)).
		SetErrorType(string(l.ErrorType)).
		SetLatencyMs(l.LatencyMS).
		SetInputTokens(l.InputTokens).
		SetOutputTokens(l.OutputTokens).
		SetTotalTokens(l.TotalTokens).
		SetCacheReadTokens(l.CacheReadTokens).
		SetCacheCreationTokens(l.CacheCreationTokens).
		SetCallCount(l.CallCount).
		SetCost(l.Cost).
		SetRawCost(l.RawCost).
		SetAboveHit(l.AboveHit).
		SetOverdraft(l.Overdraft).
		SetBilled(l.Billed).
		SetCreatedAt(l.CreatedAt)
	// client_ip（S-E 2026-08-17）：非空才 Set（ent 只落被 Set 的列——空 = NULL
	// 不写该列，与 COPY 路径 usageLogRowValues 条件赋值一一对应）。
	if l.ClientIP != "" {
		c = c.SetClientIP(l.ClientIP)
	}
	if l.ClientIPSource != "" {
		c = c.SetClientIPSource(l.ClientIPSource)
	}
	if l.ClientIPTrusted != nil {
		c = c.SetClientIPTrusted(*l.ClientIPTrusted)
	}
	if l.GroupID > 0 {
		c = c.SetGroupID(l.GroupID)
	}
	if l.AccountID > 0 {
		c = c.SetAccountID(l.AccountID)
	}
	if l.TemplateID > 0 {
		c = c.SetTemplateID(l.TemplateID)
	}
	if l.TargetKind != "" {
		c = c.SetTargetKind(l.TargetKind)
	}
	if l.UpstreamID > 0 {
		c = c.SetUpstreamID(l.UpstreamID)
	}
	if l.UpstreamName != "" {
		c = c.SetUpstreamName(l.UpstreamName)
	}
	if l.UpstreamHost != "" {
		c = c.SetUpstreamHost(l.UpstreamHost)
	}
	if l.UpstreamMultiplierBP != nil {
		c = c.SetUpstreamMultiplierBp(*l.UpstreamMultiplierBP)
	}
	if l.UserID > 0 {
		c = c.SetUserID(l.UserID)
	}
	if l.KeyID > 0 {
		c = c.SetKeyID(l.KeyID)
	}
	if l.MappedModel != "" {
		c = c.SetMappedModel(l.MappedModel)
	}
	if l.BillingTier != "" {
		c = c.SetBillingTier(l.BillingTier)
	}
	if l.TTFTMS != nil {
		c = c.SetTtftMs(*l.TTFTMS)
	}
	if l.PriceInputMillis != nil {
		c = c.SetPriceInputMillis(*l.PriceInputMillis)
	}
	if l.PriceOutputMillis != nil {
		c = c.SetPriceOutputMillis(*l.PriceOutputMillis)
	}
	if l.PriceCacheReadMillis != nil {
		c = c.SetPriceCacheReadMillis(*l.PriceCacheReadMillis)
	}
	if l.PriceCacheCreationMillis != nil {
		c = c.SetPriceCacheCreationMillis(*l.PriceCacheCreationMillis)
	}
	if l.PricePerCallMillis != nil {
		c = c.SetPricePerCallMillis(*l.PricePerCallMillis)
	}
	if l.UpstreamCost != nil {
		c = c.SetUpstreamCost(*l.UpstreamCost)
	}
	if l.GrossProfit != nil {
		c = c.SetGrossProfit(*l.GrossProfit)
	}
	if l.ProfitMarginBP != nil {
		c = c.SetProfitMarginBp(*l.ProfitMarginBP)
	}
	return c
}

// usageLogCopyColumns COPY 列清单 = buildUsageLogCreate 设置的列集合（31 列
// 全列显式列出——未设置的可选列传 NULL，与 ent 省略列（→NULL）等价；列序
// 与 usage_logs 分区表列定义一致，5 索引兼容）。COPY 无 65535 参数上限，
// 整事务一次 COPY（无分片）。统一计费模型（spec 2026-08-13）：原图片 6 列
// （image tokens/count + 3 价格快照）已删，加 call_count/price_per_call_millis。
// S-E（2026-08-17）：加 client_ip（紧随 request_id，与分区表列定义一致）。
// spec 2026-08-18：加 raw_cost（紧随 cost——恒落可 0，对齐 cost 恒落语义）。
// F2 ledger-cursor（spec 2026-08-23）：加 billed（紧随 overdraft——与分区表
// 列定义同位；恒落布尔，出生标记由调用方盖章）。（自 billing_repo.go 整体
// 搬迁：COPY 事实源归 usage 写入面所有，billing_repo.go 归 F2 T3 独占。）
var usageLogCopyColumns = []string{
	usagelog.FieldRequestID, usagelog.FieldClientIP, usagelog.FieldClientIPSource,
	usagelog.FieldClientIPTrusted, usagelog.FieldGroupID, usagelog.FieldAccountID,
	usagelog.FieldTemplateID, usagelog.FieldTargetKind, usagelog.FieldUpstreamID,
	usagelog.FieldUpstreamName, usagelog.FieldUpstreamHost, usagelog.FieldUpstreamMultiplierBp,
	usagelog.FieldUserID,
	usagelog.FieldKeyID, usagelog.FieldModel, usagelog.FieldMappedModel,
	usagelog.FieldFormat, usagelog.FieldErrorType, usagelog.FieldLatencyMs,
	usagelog.FieldTtftMs, usagelog.FieldInputTokens, usagelog.FieldPriceInputMillis,
	usagelog.FieldOutputTokens, usagelog.FieldPriceOutputMillis, usagelog.FieldTotalTokens,
	usagelog.FieldCacheReadTokens, usagelog.FieldPriceCacheReadMillis,
	usagelog.FieldCacheCreationTokens, usagelog.FieldPriceCacheCreationMillis,
	usagelog.FieldCallCount, usagelog.FieldPricePerCallMillis, usagelog.FieldCost,
	usagelog.FieldRawCost, usagelog.FieldUpstreamCost, usagelog.FieldGrossProfit,
	usagelog.FieldProfitMarginBp,
	usagelog.FieldBillingTier, usagelog.FieldAboveHit, usagelog.FieldOverdraft,
	usagelog.FieldBilled,
	usagelog.FieldCreatedAt,
}

// usageLogRowValues 单行 COPY 值（与 buildUsageLogCreate 的 Set 条件一一对应：
// 可选列 >0/非空/非 nil 才赋值，否则 NULL；call_count 恒落（NOT NULL DEFAULT 0）；
// cost/raw_cost 恒落（spec 2026-08-18——乘倍率前原始成本，可 0）；
// client_ip 非空才赋值，否则 NULL；billed 恒落布尔——出生标记透传）。
func usageLogRowValues(l *domain.UsageLog) []any {
	var groupID, accountID, templateID, userID, keyID, mappedModel, billingTier, clientIP any
	var clientIPSource, clientIPTrusted, targetKind, upstreamID, upstreamName, upstreamHost, upstreamMultiplier any
	var ttft, priceIn, priceOut, priceCR, priceCC, pricePerCall, upstreamCost, grossProfit, profitMargin any
	if l.ClientIP != "" {
		clientIP = l.ClientIP
	}
	if l.ClientIPSource != "" {
		clientIPSource = l.ClientIPSource
	}
	if l.ClientIPTrusted != nil {
		clientIPTrusted = *l.ClientIPTrusted
	}
	if l.GroupID > 0 {
		groupID = l.GroupID
	}
	if l.AccountID > 0 {
		accountID = l.AccountID
	}
	if l.TemplateID > 0 {
		templateID = l.TemplateID
	}
	if l.TargetKind != "" {
		targetKind = l.TargetKind
	}
	if l.UpstreamID > 0 {
		upstreamID = l.UpstreamID
	}
	if l.UpstreamName != "" {
		upstreamName = l.UpstreamName
	}
	if l.UpstreamHost != "" {
		upstreamHost = l.UpstreamHost
	}
	if l.UpstreamMultiplierBP != nil {
		upstreamMultiplier = *l.UpstreamMultiplierBP
	}
	if l.UserID > 0 {
		userID = l.UserID
	}
	if l.KeyID > 0 {
		keyID = l.KeyID
	}
	if l.MappedModel != "" {
		mappedModel = l.MappedModel
	}
	if l.BillingTier != "" {
		billingTier = l.BillingTier
	}
	if l.TTFTMS != nil {
		ttft = *l.TTFTMS
	}
	if l.PriceInputMillis != nil {
		priceIn = *l.PriceInputMillis
	}
	if l.PriceOutputMillis != nil {
		priceOut = *l.PriceOutputMillis
	}
	if l.PriceCacheReadMillis != nil {
		priceCR = *l.PriceCacheReadMillis
	}
	if l.PriceCacheCreationMillis != nil {
		priceCC = *l.PriceCacheCreationMillis
	}
	if l.PricePerCallMillis != nil {
		pricePerCall = *l.PricePerCallMillis
	}
	if l.UpstreamCost != nil {
		upstreamCost = *l.UpstreamCost
	}
	if l.GrossProfit != nil {
		grossProfit = *l.GrossProfit
	}
	if l.ProfitMarginBP != nil {
		profitMargin = *l.ProfitMarginBP
	}
	return []any{
		l.RequestID, clientIP, clientIPSource, clientIPTrusted,
		groupID, accountID, templateID, targetKind, upstreamID, upstreamName, upstreamHost, upstreamMultiplier,
		userID, keyID,
		l.Model, mappedModel, string(l.Format), string(l.ErrorType), l.LatencyMS, ttft,
		l.InputTokens, priceIn, l.OutputTokens, priceOut, l.TotalTokens,
		l.CacheReadTokens, priceCR, l.CacheCreationTokens, priceCC,
		l.CallCount, pricePerCall,
		l.Cost, l.RawCost, upstreamCost, grossProfit, profitMargin,
		billingTier, l.AboveHit, l.Overdraft, l.Billed, l.CreatedAt,
	}
}

// usageLogsSummarySQL builds the aggregate query from the same filtering
// surface as QueryUsages. Cursor and limit are intentionally excluded: the
// summary always covers the complete active filter window.
func usageLogsSummarySQL(q UsageQuery) (string, []any) {
	// PostgreSQL sum(bigint) returns numeric. Casting the sum (or the
	// subtraction) directly to bigint can fail once an aggregate exceeds the
	// ledger range, even though each stored row is valid. Aggregate in numeric,
	// clamp every exposed integer to int64, and only then cast for the API. Keep
	// the aggregate directly over usage_logs so the dynamic filters below remain
	// predicates on the source rows rather than on an outer summary row.
	const selectSQL = `SELECT
		count(*)::bigint,
		count(upstream_cost)::bigint,
		CASE
			WHEN COALESCE(sum(cost::numeric), 0::numeric) > 9223372036854775807::numeric THEN 9223372036854775807::numeric
			WHEN COALESCE(sum(cost::numeric), 0::numeric) < -9223372036854775808::numeric THEN -9223372036854775808::numeric
			ELSE COALESCE(sum(cost::numeric), 0::numeric)
		END::bigint,
		CASE
			WHEN COALESCE(sum(cost::numeric) FILTER (WHERE upstream_cost IS NOT NULL), 0::numeric) > 9223372036854775807::numeric THEN 9223372036854775807::numeric
			WHEN COALESCE(sum(cost::numeric) FILTER (WHERE upstream_cost IS NOT NULL), 0::numeric) < -9223372036854775808::numeric THEN -9223372036854775808::numeric
			ELSE COALESCE(sum(cost::numeric) FILTER (WHERE upstream_cost IS NOT NULL), 0::numeric)
		END::bigint,
		CASE
			WHEN sum(upstream_cost::numeric) IS NULL THEN NULL
			WHEN sum(upstream_cost::numeric) > 9223372036854775807::numeric THEN 9223372036854775807::numeric
			WHEN sum(upstream_cost::numeric) < -9223372036854775808::numeric THEN -9223372036854775808::numeric
			ELSE sum(upstream_cost::numeric)
		END::bigint,
		CASE
			WHEN sum(cost::numeric - upstream_cost::numeric) IS NULL THEN NULL
			WHEN sum(cost::numeric - upstream_cost::numeric) > 9223372036854775807::numeric THEN 9223372036854775807::numeric
			WHEN sum(cost::numeric - upstream_cost::numeric) < -9223372036854775808::numeric THEN -9223372036854775808::numeric
			ELSE sum(cost::numeric - upstream_cost::numeric)
		END::bigint,
		CASE
			WHEN COALESCE(sum(cost::numeric) FILTER (WHERE upstream_cost IS NOT NULL), 0::numeric) = 0
				OR sum(cost::numeric - upstream_cost::numeric) IS NULL THEN NULL
			WHEN round(sum(cost::numeric - upstream_cost::numeric) * 10000 /
				sum(cost::numeric) FILTER (WHERE upstream_cost IS NOT NULL)) > 9223372036854775807::numeric THEN 9223372036854775807::numeric
			WHEN round(sum(cost::numeric - upstream_cost::numeric) * 10000 /
				sum(cost::numeric) FILTER (WHERE upstream_cost IS NOT NULL)) < -9223372036854775808::numeric THEN -9223372036854775808::numeric
			ELSE round(sum(cost::numeric - upstream_cost::numeric) * 10000 /
				sum(cost::numeric) FILTER (WHERE upstream_cost IS NOT NULL))
		END::bigint,
		count(*) FILTER (WHERE upstream_cost IS NOT NULL AND cost::numeric - upstream_cost::numeric < 0)::bigint
	FROM usage_logs`

	clauses := make([]string, 0, 9)
	args := make([]any, 0, 9)
	addEqual := func(column string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if q.GroupID > 0 {
		addEqual("group_id", q.GroupID)
	}
	if q.AccountID > 0 {
		addEqual("account_id", q.AccountID)
	}
	if q.UserID > 0 {
		addEqual("user_id", q.UserID)
	}
	if q.KeyID > 0 {
		addEqual("key_id", q.KeyID)
	}
	if q.Model != "" {
		addEqual("model", q.Model)
	}
	if q.Format != "" {
		addEqual("format", q.Format)
	}
	if q.ErrorType != "" {
		addEqual("error_type", q.ErrorType)
	}
	if q.From != nil {
		args = append(args, *q.From)
		clauses = append(clauses, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if q.To != nil {
		args = append(args, *q.To)
		clauses = append(clauses, fmt.Sprintf("created_at < $%d", len(args)))
	}
	if len(clauses) == 0 {
		return selectSQL, args
	}
	return selectSQL + "\nWHERE " + strings.Join(clauses, " AND "), args
}

// SummarizeUsages aggregates the complete UsageQuery filter window. It uses
// the persisted request-time estimate snapshots and never derives totals from
// a paginated result set.
func (r *UsageRepo) SummarizeUsages(ctx context.Context, q UsageQuery) (*UsageLogsSummary, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("usage repo: pgx pool not configured (repository.NewWithPG); cannot summarize usage logs")
	}
	query, args := usageLogsSummarySQL(q)
	out := &UsageLogsSummary{}
	if err := r.pool.QueryRow(ctx, query, args...).Scan(
		&out.RequestCount,
		&out.CostedRequestCount,
		&out.UserCharge,
		&out.AttributedUserCharge,
		&out.UpstreamCost,
		&out.GrossProfit,
		&out.ProfitMarginBP,
		&out.LossRequestCount,
	); err != nil {
		return nil, err
	}
	return out, nil
}

// QueryUsages usage_logs keyset 游标分页查询（用户裁决：无 from/to 的全分区
// OFFSET 扫描是压测中危——游标分页 + from/to 必填 + 零新索引，id 主键天然有序）。
// 游标语义：WHERE id < cursor AND created_at >= from AND created_at < to
// [AND 既有过滤]，
// ORDER BY id DESC LIMIT limit+1——多取 1 行探测是否有下一页（调用方按
// len(rows) > limit 组装 next_cursor）；去 Count（Total 已从契约移除）。
func (r *UsageRepo) QueryUsages(ctx context.Context, q UsageQuery) ([]*domain.UsageLog, error) {
	pred := r.client.UsageLog.Query()
	if q.GroupID > 0 {
		pred = pred.Where(usagelog.GroupIDEQ(q.GroupID))
	}
	if q.AccountID > 0 {
		pred = pred.Where(usagelog.AccountIDEQ(q.AccountID))
	}
	if q.UserID > 0 {
		pred = pred.Where(usagelog.UserIDEQ(q.UserID))
	}
	if q.KeyID > 0 {
		pred = pred.Where(usagelog.KeyIDEQ(q.KeyID))
	}
	if q.Model != "" {
		pred = pred.Where(usagelog.ModelEQ(q.Model))
	}
	if q.Format != "" {
		pred = pred.Where(usagelog.FormatEQ(usagelog.Format(q.Format)))
	}
	if q.ErrorType != "" {
		pred = pred.Where(usagelog.ErrorTypeEQ(q.ErrorType))
	}
	if q.From != nil {
		pred = pred.Where(usagelog.CreatedAtGTE(*q.From))
	}
	if q.To != nil {
		pred = pred.Where(usagelog.CreatedAtLT(*q.To))
	}
	if q.Cursor > 0 {
		pred = pred.Where(usagelog.IDLT(q.Cursor))
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	rows, err := pred.Order(ent.Desc(usagelog.FieldID)).Limit(q.Limit + 1).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.UsageLog, 0, len(rows))
	for _, row := range rows {
		l := &domain.UsageLog{
			ID: row.ID, RequestID: row.RequestID,
			Model: row.Model, Format: domain.RequestFormat(row.Format),
			// 用户裁决（err_logs 分表）：StatusCode/ErrorMessage 不再落 usage_logs
			// ——查询结果恒零值/nil（错误审计字段由 err_logs 承载）。
			ErrorType:                domain.ErrorType(row.ErrorType),
			LatencyMS:                row.LatencyMs,
			TTFTMS:                   row.TtftMs,
			InputTokens:              row.InputTokens,
			PriceInputMillis:         row.PriceInputMillis,
			OutputTokens:             row.OutputTokens,
			PriceOutputMillis:        row.PriceOutputMillis,
			TotalTokens:              row.TotalTokens,
			CacheReadTokens:          row.CacheReadTokens,
			PriceCacheReadMillis:     row.PriceCacheReadMillis,
			CacheCreationTokens:      row.CacheCreationTokens,
			PriceCacheCreationMillis: row.PriceCacheCreationMillis,
			CallCount:                row.CallCount,
			PricePerCallMillis:       row.PricePerCallMillis,
			Cost:                     row.Cost,
			RawCost:                  row.RawCost,
			UpstreamCost:             row.UpstreamCost,
			GrossProfit:              row.GrossProfit,
			ProfitMarginBP:           row.ProfitMarginBp,
			AboveHit:                 row.AboveHit,
			Overdraft:                row.Overdraft,
			CreatedAt:                row.CreatedAt,
		}
		if row.GroupID != nil {
			l.GroupID = *row.GroupID
		}
		if row.AccountID != nil {
			l.AccountID = *row.AccountID
		}
		if row.TemplateID != nil {
			l.TemplateID = *row.TemplateID
		}
		if row.TargetKind != nil {
			l.TargetKind = *row.TargetKind
		}
		if row.UpstreamID != nil {
			l.UpstreamID = *row.UpstreamID
		}
		if row.UpstreamName != nil {
			l.UpstreamName = *row.UpstreamName
		}
		if row.UpstreamHost != nil {
			l.UpstreamHost = *row.UpstreamHost
		}
		l.UpstreamMultiplierBP = row.UpstreamMultiplierBp
		if row.UserID != nil {
			l.UserID = *row.UserID
		}
		if row.KeyID != nil {
			l.KeyID = *row.KeyID
		}
		if row.MappedModel != nil {
			l.MappedModel = *row.MappedModel
		}
		if row.BillingTier != nil {
			l.BillingTier = *row.BillingTier
		}
		if row.ClientIP != nil {
			l.ClientIP = *row.ClientIP
		}
		if row.ClientIPSource != nil {
			l.ClientIPSource = *row.ClientIPSource
		}
		l.ClientIPTrusted = row.ClientIPTrusted
		out = append(out, l)
	}
	return out, nil
}

// ScanUsageAgg 批量账号 usage_logs 区间聚合（/api/admin/accounts/usage 查询面——
// 统一 usage API spec 2026-08-18）：单连接单查询，`ANY($1)` 100 ids 参数数组
// 规模内 + created_at 半开区间 [from, to)（分区键——RANGE 分区剪枝 + 既有
// account_id/created_at 索引）。SQL 侧 GROUP BY 聚合（F-P2-2 形态：服务端
// 聚合，不拉全行客户端算）；SUM 毫分 int64 原样（USD 换算在 handler 展示
// 边界）。返回 map[account_id]agg——无记录账号无键（补零由 service 层按 ids
// 全量组装）。pool 未注入（New 构造）→ 显式错误（与 StatRepo 同纪律）。
// 数量防御（N5）：>usageAggMaxAccountIDs → 显式错误（防御 handler 之外调用
// 方——ANY 参数数组规模上限）。
func (r *UsageRepo) ScanUsageAgg(ctx context.Context, accountIDs []int64, from, to time.Time) (map[int64]*domain.UsageAgg, error) {
	if len(accountIDs) > usageAggMaxAccountIDs {
		return nil, fmt.Errorf("usage repo: ScanUsageAgg: %d account ids exceed limit %d", len(accountIDs), usageAggMaxAccountIDs)
	}
	if r.pool == nil {
		return nil, fmt.Errorf("usage repo: pgx pool not configured (repository.NewWithPG); cannot scan usage agg")
	}
	// PostgreSQL sum(bigint) returns numeric. Keep the aggregate in numeric until
	// it is saturated to the API's int64 range; casting an unchecked sum directly
	// to bigint makes a valid pair of large ledger rows fail the whole endpoint.
	// The columns are intentionally cast individually so pgx can scan plain int64
	// values without introducing a numeric dependency in the domain type.
	rows, err := r.pool.Query(ctx, `SELECT account_id, count(*)::bigint,
		LEAST(GREATEST(COALESCE(sum(cost::numeric), 0::numeric), -9223372036854775808::numeric), 9223372036854775807::numeric)::bigint,
		LEAST(GREATEST(COALESCE(sum(raw_cost::numeric), 0::numeric), -9223372036854775808::numeric), 9223372036854775807::numeric)::bigint,
		LEAST(GREATEST(COALESCE(sum(total_tokens::numeric), 0::numeric), -9223372036854775808::numeric), 9223372036854775807::numeric)::bigint
		FROM usage_logs WHERE account_id = ANY($1) AND created_at >= $2 AND created_at < $3
		GROUP BY account_id`, accountIDs, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]*domain.UsageAgg, len(accountIDs))
	for rows.Next() {
		a := &domain.UsageAgg{}
		if err := rows.Scan(&a.AccountID, &a.Requests, &a.Cost, &a.RawCost, &a.TotalTokens); err != nil {
			return nil, err
		}
		out[a.AccountID] = a
	}
	return out, rows.Err()
}
