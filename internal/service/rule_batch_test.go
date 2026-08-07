package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeleteRulesBatch 批量删除规则：成功（reload +1）/ 空 ids 400 / 超长 400 /
// 重复 ids 400 / 缺 id 404（失败不 reload）+ 事务原子性。
func TestDeleteRulesBatch(t *testing.T) {
	svc, _, rl := newRuleSvc()
	ids := make([]int64, 0, 3)
	for _, p := range []int{10, 20, 30} {
		created, err := svc.CreateRule(context.Background(), RuleInput{
			Name: fmt.Sprintf("r%d", p), Enabled: true, Priority: p, When: validWhen(), Then: validThen(),
		})
		require.NoError(t, err)
		ids = append(ids, created.ID)
	}
	base := rl.calls // 3 次创建 reload

	// 成功：删前两个 → reload +1
	require.NoError(t, svc.DeleteRulesBatch(context.Background(), ids[:2]))
	require.Equal(t, base+1, rl.calls, "批量删除成功后必须触发引擎 Reload")
	rows, total, err := svc.ListRules(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, ids[2], rows[0].ID)

	// 空 ids → 400 语义（不 reload）
	require.ErrorIs(t, svc.DeleteRulesBatch(context.Background(), nil), ErrInvalidInput)
	require.Equal(t, base+1, rl.calls, "校验失败不触发 Reload")

	// 超长（101 条）→ 400 语义
	long := make([]int64, 101)
	for i := range long {
		long[i] = int64(i + 1)
	}
	require.ErrorIs(t, svc.DeleteRulesBatch(context.Background(), long), ErrInvalidInput)
	require.Equal(t, base+1, rl.calls, "校验失败不触发 Reload")

	// 重复 ids → 400 语义（service validateIDs 兜底，handler 已去重）
	require.ErrorIs(t, svc.DeleteRulesBatch(context.Background(), []int64{ids[2], ids[2]}), ErrInvalidInput)
	require.Equal(t, base+1, rl.calls, "校验失败不触发 Reload")

	// 缺 id → ErrNotFound 含缺失 id，失败不 reload
	err = svc.DeleteRulesBatch(context.Background(), []int64{ids[2], 999})
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing", "404 消息含 id")
	require.Equal(t, base+1, rl.calls, "失败不触发 Reload")

	// 事务原子性：缺 id 时已存在 id 不被删除（fake 先检查后删，与真实 repo 同语义）
	rows, total, err = svc.ListRules(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, ids[2], rows[0].ID, "批量失败不删除任何规则")
}
