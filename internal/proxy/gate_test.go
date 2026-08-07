package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
)

// --- concurrencyGate 单元测试（内存原子语义） ---

// 两级 acquire（user → key）与两步回滚（评审 I-3）：user 成功 key 失败 →
// user 计数复原，防泄漏。
func TestGateAcquireReleaseAndRollback(t *testing.T) {
	g := newConcurrencyGate()
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 2, KeyMaxConc: 1}
	g.upsert(meta) // 种子：计数器存在后才参与门禁
	lvl1, ok := g.acquire(meta)
	require.True(t, ok)
	require.Equal(t, 3, lvl1, "user+key 两层全占")

	// 第二请求：user 层可占（2 内）但 key 层超限 → 整体失败 + user 回滚
	lvl2, ok := g.acquire(meta)
	require.False(t, ok)
	require.Zero(t, lvl2)
	snap := g.store.Load()
	require.Equal(t, int64(1), snap.users[1].Load(), "user 计数已回滚（1 非 2）")
	require.Equal(t, int64(1), snap.keys[1].Load())

	g.release(meta, lvl1)
	require.Equal(t, int64(0), snap.users[1].Load())
	require.Equal(t, int64(0), snap.keys[1].Load())

	lvl3, ok := g.acquire(meta)
	require.True(t, ok, "释放后可再占")
	g.release(meta, lvl3)
}

func TestGateUserLimit(t *testing.T) {
	g := newConcurrencyGate()
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 1}
	g.upsert(meta)
	lvl, ok := g.acquire(meta)
	require.True(t, ok)
	require.Equal(t, 1, lvl, "仅 user 层")
	_, ok = g.acquire(meta)
	require.False(t, ok, "user 并发超限")
	g.release(meta, lvl)
	_, ok = g.acquire(meta)
	require.True(t, ok, "release 后可再占")
}

// 快照换入换出：在途并发/额度跨 reload 继承（复用 scheduler reload 继承教训；
// 评审提醒②：quota_used 内存值同走继承）。
func TestGateReloadInheritsInflight(t *testing.T) {
	g := newConcurrencyGate()
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 4, KeyMaxConc: 4, HasQuota: true, Quota: 100}
	g.reload(map[string]domain.KeyMeta{"h": meta}) // 种子
	lvl, ok := g.acquire(meta)
	require.True(t, ok)
	g.deductQuota(1, 30)

	g.reload(map[string]domain.KeyMeta{"h": meta})
	snap := g.store.Load()
	require.Equal(t, int64(1), snap.users[1].Load(), "user 在途继承")
	require.Equal(t, int64(1), snap.keys[1].Load(), "key 在途继承")
	require.Equal(t, int64(30), snap.quotas[1].Load(), "额度在途继承")

	// 跨 reload 的 release/deduct 命中新快照的继承值
	g.release(meta, lvl)
	g.deductQuota(1, 10)
	require.Equal(t, int64(0), snap.users[1].Load())
	require.Equal(t, int64(40), snap.quotas[1].Load())
}

// 额度：检查纯读（无计数副作用）、后扣、无额度 key 零成本短路。
func TestGateQuotaCheckAndDeduct(t *testing.T) {
	g := newConcurrencyGate()
	noQuota := domain.KeyMeta{KeyID: 2, HasQuota: false}
	require.False(t, g.quotaExhausted(noQuota), "无额度 key 短路")
	g.deductQuota(2, 50) // 无条目 → no-op（恒 0）

	quota := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100}
	g.reload(map[string]domain.KeyMeta{"q": quota}) // 种子（额度条目存在）
	require.False(t, g.quotaExhausted(quota))
	g.deductQuota(1, 60)
	require.False(t, g.quotaExhausted(quota))
	g.deductQuota(1, 50)
	require.True(t, g.quotaExhausted(quota), "后扣超限拦下一个请求")

	// 检查无副作用：quotaExhausted 不改变计数
	before := g.store.Load().quotas[1].Load()
	_ = g.quotaExhausted(quota)
	require.Equal(t, before, g.store.Load().quotas[1].Load(), "检查是纯读（评审提醒①）")
}

// reload 后无额度 key 不建 quota 条目；reload 新 key 从快照值起算。
func TestGateReloadQuotaBase(t *testing.T) {
	g := newConcurrencyGate()
	noQuota := domain.KeyMeta{KeyID: 2, HasQuota: false}
	g.reload(map[string]domain.KeyMeta{"nq": noQuota})
	snap := g.store.Load()
	_, ok := snap.quotas[2]
	require.False(t, ok, "无额度 key 无条目")

	withQuota := domain.KeyMeta{KeyID: 3, HasQuota: true, Quota: 50, QuotaUsed: 7}
	g.reload(map[string]domain.KeyMeta{"q": withQuota})
	require.Equal(t, int64(7), g.store.Load().quotas[3].Load(), "新 key 基线 = DB 快照值")
}

// upsert/delete 增量：新 key 建条目、已存在不动在途值、删除清条目。
func TestGateUpsertDelete(t *testing.T) {
	g := newConcurrencyGate()
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, HasQuota: true, Quota: 10, QuotaUsed: 3}
	g.upsert(meta)
	require.Equal(t, int64(3), g.store.Load().quotas[1].Load(), "新 key 额度基线")
	g.deductQuota(1, 5)
	g.upsert(meta)
	require.Equal(t, int64(8), g.store.Load().quotas[1].Load(), "upsert 不动在途额度")

	g.delete(1)
	snap := g.store.Load()
	_, ok := snap.keys[1]
	require.False(t, ok, "key 计数移除")
	_, ok = snap.quotas[1]
	require.False(t, ok, "额度条目移除")
}

// 缺 key 的 key 层失败（reload 后无该 key 计数器 → 该层跳过，不误拒）。
func TestGateMissingCounterFailOpen(t *testing.T) {
	g := newConcurrencyGate()
	meta := domain.KeyMeta{KeyID: 99, UserID: 99, UserMaxConc: 1, KeyMaxConc: 1}
	lvl, ok := g.acquire(meta)
	require.True(t, ok, "计数器缺失层跳过（fail-open）")
	g.release(meta, lvl)
}
