package repository

// P1（压测 2026-08-11 修复）对齐锚：usageLogAlignColumnDDLs 的列名必须存在于
// 静态建表 DDL usageLogCreateDDL——两处定义若漂移会漏补列（旧库升级后仍缺列，
// billing 继续 42703）。

import (
	"regexp"
	"strings"
	"testing"
)

func TestUsageLogAlignColumnsMatchCreateDDL(t *testing.T) {
	colRe := regexp.MustCompile(`^ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS (\w+) `)
	for _, ddl := range usageLogAlignColumnDDLs {
		m := colRe.FindStringSubmatch(ddl)
		if m == nil {
			t.Fatalf("无法解析 align DDL 列名: %s", ddl)
		}
		if !strings.Contains(usageLogCreateDDL, m[1]) {
			t.Errorf("align 列 %s 不在 usageLogCreateDDL（静态建表 DDL）中", m[1])
		}
	}
}
