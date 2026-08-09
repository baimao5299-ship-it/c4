package repository_test

// 错误文本落盘（部署故障修复 #20）真实 PG 测试：bootstrap 建表含
// error_message 列；有值/空值（NULL）roundtrip（QueryLogs 读回）。
//
// 基座约定同 pg_partition_test.go：newPGRepos 每测试 DROP SCHEMA 重建 +
// migrate（钩子跳过 usagelog）+ 分区 bootstrap（DDL 含 error_message 列）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

func TestUsageLogErrorMessageRoundtripPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	// bootstrap 建表必须含 error_message 列（varchar，无默认值 = NULL 可空）
	var dataType string
	var isNullable string
	err := pool.QueryRow(ctx, `
		SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'usage_logs' AND column_name = 'error_message'`).
		Scan(&dataType, &isNullable)
	require.NoError(t, err, "bootstrap 建表必须含 error_message 列")
	require.Equal(t, "character varying", dataType)
	require.Equal(t, "YES", isNullable)

	// 有值 roundtrip：600 字符 → 域内截断 500 落库读回
	truncated := domain.TruncateErrMsg(strings.Repeat("x", 600))
	require.Len(t, truncated, domain.ErrMsgMaxLen)
	l1 := usageLogFor("err-msg-1", time.Now().UTC())
	l1.ErrorMessage = &truncated
	// 空值 roundtrip：ErrorMessage nil → 列 NULL（成功路径语义，SQL 不写该列）
	l2 := usageLogFor("err-msg-2", time.Now().UTC())
	require.NoError(t, repos.Logs.InsertBatch(ctx, []*domain.UsageLog{l1, l2}))

	rows, total, err := repos.QueryLogs(ctx, repository.LogQuery{Limit: 100})
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	got := map[string]*string{}
	for _, r := range rows {
		got[r.RequestID] = r.ErrorMessage
	}
	require.NotNil(t, got["err-msg-1"], "有值必须读回")
	require.Equal(t, truncated, *got["err-msg-1"])
	require.Nil(t, got["err-msg-2"], "未设置 ErrorMessage → NULL")

	// SQL 层直查确认 NULL 语义（不经 domain 映射）
	var raw *string
	err = pool.QueryRow(ctx, `SELECT error_message FROM usage_logs WHERE request_id = 'err-msg-2'`).Scan(&raw)
	require.NoError(t, err)
	require.Nil(t, raw, "DB 层 error_message 为 NULL")
	var rawVal string
	err = pool.QueryRow(ctx, `SELECT error_message FROM usage_logs WHERE request_id = 'err-msg-1'`).Scan(&rawVal)
	require.NoError(t, err)
	require.Equal(t, truncated, rawVal)
}
