package repository

// P1（压测 2026-08-11 修复）对齐锚（评审 I-1 双向化）：静态建表 DDL 与幂等补列
// ALTER 的列集合必须双向相等——单向断言（align⊆create）防不住"未来向静态 DDL
// 加列忘加 align"这一 P1 同型复发模式。两处均由 usageLogColumnDefs 单一事实源
// 生成（结构性无漂移），本断言兜底任一侧被绕过/手改。

import (
	"regexp"
	"sort"
	"strings"
	"testing"

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
}
