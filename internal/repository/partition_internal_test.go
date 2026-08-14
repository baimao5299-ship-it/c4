// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// P1（压测 2026-08-11 修复）对齐锚（评审 I-1 双向化）：静态建表 DDL 与幂等补列
// ALTER 的列集合必须双向相等——单向断言（align⊆create）防不住"未来向静态 DDL
// 加列忘加 align"这一 P1 同型复发模式。两处均由 usageLogColumnDefs 单一事实源
// 生成（结构性无漂移），本断言兜底任一侧被绕过/手改。

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// ddlColumnNames 提取建表 DDL 的列名（行首标识符，跳过 CREATE/PRIMARY 非列
// 行）与补列 ALTER 的列名（IF NOT EXISTS 后标识符），排序后返回。
func ddlColumnNames(ddls ...string) []string {
	lineRe := regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\b`)
	alterRe := regexp.MustCompile(`ADD COLUMN IF NOT EXISTS ([a-z_][a-z0-9_]*)`)
	var names []string
	for _, ddl := range ddls {
		if m := alterRe.FindStringSubmatch(ddl); m != nil {
			names = append(names, m[1])
			continue
		}
		for _, line := range strings.Split(ddl, "\n") {
			m := lineRe.FindStringSubmatch(line)
			if m == nil || m[1] == "create" || m[1] == "primary" {
				continue
			}
			names = append(names, m[1])
		}
	}
	sort.Strings(names)
	return names
}

func TestUsageLogAlignColumnsMatchCreateDDL(t *testing.T) {
	source := ddlColumnNames(strings.Join(usageLogColumnDefs, "\n"))
	create := ddlColumnNames(usageLogCreateDDL)
	align := ddlColumnNames(usageLogAlignColumnDDLs...)
	require.NotEmpty(t, source, "事实源列集合非空")
	require.Equal(t, source, create, "建表 DDL 与事实源列集合一致")
	require.Equal(t, source, align, "补列 ALTER 与事实源列集合一致")
	require.Equal(t, create, align, "建表 DDL 列集合 == 补列 ALTER 列集合（双向相等）")
	require.Contains(t, source, "price_input_millis", "对齐锚必须覆盖 P1 缺列")
	// 统一计费模型（spec 2026-08-13）：删 6 加 2——新列锚 + 旧列取反断言
	//（任何同步点残留图片 6 列即红）。
	require.Contains(t, source, "call_count", "对齐锚必须覆盖 call_count 新列")
	require.Contains(t, source, "price_per_call_millis", "对齐锚必须覆盖 price_per_call_millis 新列")
	for _, old := range []string{"image_input_tokens", "image_output_tokens", "image_count",
		"price_image_input_millis", "price_image_output_millis", "price_per_image_millis"} {
		require.NotContains(t, source, old, "删 6 列：%s 不得残留", old)
	}
}

// TestErrLogAlignColumnsMatchCreateDDL err_logs 对齐锚（架构审查 S2——P1 同型
// 复发防线：errLogColumnDefs 是第二列事实源，建表 DDL 与幂等补列 ALTER 双向
// 相等；防"向静态 DDL 加列忘加 align"）。
func TestErrLogAlignColumnsMatchCreateDDL(t *testing.T) {
	source := ddlColumnNames(strings.Join(errLogColumnDefs, "\n"))
	create := ddlColumnNames(errLogCreateDDL)
	align := ddlColumnNames(errLogAlignColumnDDLs...)
	require.NotEmpty(t, source, "事实源列集合非空")
	require.Equal(t, source, create, "建表 DDL 与事实源列集合一致")
	require.Equal(t, source, align, "补列 ALTER 与事实源列集合一致")
	require.Equal(t, create, align, "建表 DDL 列集合 == 补列 ALTER 列集合（双向相等）")
	require.Contains(t, source, "error_message", "对齐锚必须覆盖 err_logs 错误审计列")
	require.Contains(t, source, "billing_tier", "对齐锚必须覆盖 I-3 tier 审计列")
	require.NotContains(t, source, "cost", "err_logs 瘦表无计费列")
	require.NotContains(t, source, "input_tokens", "err_logs 瘦表无 token 列")
}

// TestUsageStatsAlignColumnsMatchCreateDDL usage_stats 对齐锚（用户裁决
// 2026-08-11 三表统一分区机制——usageStatsColumnDefs 第三列事实源，建表 DDL
// 与幂等补列 ALTER 双向相等；防 P1 同型复发）→ spec 2026-08-14 表重建：
// 存量库必须重建（DDL 变更大：删 total_latency_ms、加 call_count/ttft_* 六列 +
// bigint[] 数组列），幂等补列 ALTER 机制随之删除——align 集合必须恒空（防
// 复活回归），建表 DDL 仍与事实源双向相等。
func TestUsageStatsAlignColumnsMatchCreateDDL(t *testing.T) {
	source := ddlColumnNames(strings.Join(usageStatsColumnDefs, "\n"))
	create := ddlColumnNames(usageStatsCreateDDL)
	require.NotEmpty(t, source, "事实源列集合非空")
	require.Equal(t, source, create, "建表 DDL 与事实源列集合一致")
	require.Empty(t, usageStatsAlignColumnDDLs, "usage_stats 不做幂等补列（表重建，存量库必须重建）")
	require.Contains(t, source, "bucket_time", "对齐锚必须覆盖分区键 bucket_time")
	require.Contains(t, source, "cost", "对齐锚必须覆盖计费预聚合列")
	require.Contains(t, source, "call_count", "表重建必须含按次调用列")
	require.Contains(t, source, "ttft_hist", "表重建必须含 TTFT 直方图数组列（ent carve-out）")
	require.NotContains(t, source, "total_latency_ms", "表重建删除总延迟列（TTFT 直方图替代）")
}

// TestIsBootstrapRaceErrorCodeSet 并发 bootstrap 容忍码集锚：撞名
// （isDuplicateObject——42P07 关系插入撞锁 / 42710 类型名预检撞隐式复合类型 /
// 23505 CREATE SEQUENCE 并发）+ 缺失（isMissingObject——42P01，stale-DROP 窗
// 口）全部容忍；其余错误码与非 pgconn 错误不误判。漏 42710/42P01 均导致
// TestUsageLogPartitionConcurrentBootstrapPG 偶发失败（2026-08-13 实测两种码
// 均现）。
func TestIsBootstrapRaceErrorCodeSet(t *testing.T) {
	pgErr := func(code string) error {
		return &pgconn.PgError{Code: code}
	}
	// 撞名集
	require.True(t, isDuplicateObject(pgErr("42P07")), "CREATE TABLE 撞关系 42P07")
	require.True(t, isDuplicateObject(pgErr("42710")), "CREATE TABLE 撞隐式复合类型 42710")
	require.True(t, isDuplicateObject(pgErr("23505")), "CREATE SEQUENCE 并发 23505")
	// 缺失集（stale-DROP 窗口）
	require.True(t, isMissingObject(pgErr("42P01")), "OWNED BY/索引/对齐/分区引用缺失的表或序列 42P01")
	require.False(t, isMissingObject(pgErr("42P07")), "撞名非缺失不得归入 42P01 集")
	// 组合判定
	require.True(t, isBootstrapRaceError(pgErr("42P07")), "撞名 → bootstrap 竞态")
	require.True(t, isBootstrapRaceError(pgErr("42710")), "类型撞名 → bootstrap 竞态")
	require.True(t, isBootstrapRaceError(pgErr("23505")), "序列并发 → bootstrap 竞态")
	require.True(t, isBootstrapRaceError(pgErr("42P01")), "窗口缺失 → bootstrap 竞态")
	require.False(t, isBootstrapRaceError(pgErr("42P02")), "其它错误码不误判")
	require.False(t, isBootstrapRaceError(errors.New("network error")), "非 pgconn 错误不误判")
}
