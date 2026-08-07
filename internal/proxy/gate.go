package proxy

import (
	"sync/atomic"

	"go-proxy-mini/internal/domain"
)

// concurrencyGate 两级并发/额度内存门禁：user/key 在途计数 + key 额度已消耗
// 计数。快照原子换入换出（reload 时重建），在途/已扣值跨 reload 继承
// （复用 scheduler reload 继承教训：跨 reload 的 Release/deduct 命中新快照
// 的继承值，计数不丢不拉负）。热路径零 DB、零锁（仅 atomic）。
//
// 无额度 key（quota=0）不建 quota 条目——HasQuota 短路：检查与扣减均走
// 计数器存在性，路径与现状（无门禁）成本相当（1 次快照读 + map 查）。
type concurrencyGate struct {
	store atomic.Pointer[gateSnapshot]
}

type gateSnapshot struct {
	users  map[int64]*atomic.Int64 // user_id → 在途请求数
	keys   map[int64]*atomic.Int64 // key_id → 在途请求数
	quotas map[int64]*atomic.Int64 // key_id → 已消耗额度（内存权威；无额度 key 无条目）
}

func newConcurrencyGate() *concurrencyGate {
	g := &concurrencyGate{}
	g.store.Store(&gateSnapshot{
		users:  make(map[int64]*atomic.Int64),
		keys:   make(map[int64]*atomic.Int64),
		quotas: make(map[int64]*atomic.Int64),
	})
	return g
}

// reload 从鉴权快照重建计数器；在途值跨 reload 继承（旧快照与新快照共有的
// user/key 计数平移——跨 reload 的 Release/deduct 命中新快照继承值）。
func (g *concurrencyGate) reload(metas map[string]domain.KeyMeta) {
	snap := &gateSnapshot{
		users:  make(map[int64]*atomic.Int64, len(metas)),
		keys:   make(map[int64]*atomic.Int64, len(metas)),
		quotas: make(map[int64]*atomic.Int64),
	}
	old := g.store.Load()
	for _, meta := range metas {
		if _, ok := snap.users[meta.UserID]; !ok {
			c := &atomic.Int64{}
			if o, ok := old.users[meta.UserID]; ok {
				c.Store(o.Load()) // 在途继承
			}
			snap.users[meta.UserID] = c
		}
		kc := &atomic.Int64{}
		if o, ok := old.keys[meta.KeyID]; ok {
			kc.Store(o.Load())
		}
		snap.keys[meta.KeyID] = kc
		if meta.HasQuota {
			q := &atomic.Int64{}
			if o, ok := old.quotas[meta.KeyID]; ok {
				q.Store(o.Load()) // 在途额度继承（评审提醒②）
			} else {
				q.Store(meta.QuotaUsed)
			}
			snap.quotas[meta.KeyID] = q
		}
	}
	g.store.Store(snap)
}

// upsert 增量刷新单 key 计数器（创建/轮换后；已存在条目不动在途值；
// 低频管理路径，重建快照可接受）。
func (g *concurrencyGate) upsert(meta domain.KeyMeta) {
	old := g.store.Load()
	snap := &gateSnapshot{
		users:  cloneCounters(old.users),
		keys:   cloneCounters(old.keys),
		quotas: cloneCounters(old.quotas),
	}
	if _, ok := snap.users[meta.UserID]; !ok {
		snap.users[meta.UserID] = &atomic.Int64{}
	}
	if _, ok := snap.keys[meta.KeyID]; !ok {
		snap.keys[meta.KeyID] = &atomic.Int64{}
	}
	if meta.HasQuota {
		if _, ok := snap.quotas[meta.KeyID]; !ok {
			q := &atomic.Int64{}
			q.Store(meta.QuotaUsed)
			snap.quotas[meta.KeyID] = q
		}
	}
	g.store.Store(snap)
}

// delete 移除 key 计数器与额度条目（user 计数保留——紧随其后的 invalidate
// → Reload 会按剩余 key 重建；删除到 0 的用户在下一次 reload 收敛）。
func (g *concurrencyGate) delete(keyID int64) {
	old := g.store.Load()
	if _, ok := old.keys[keyID]; !ok {
		return
	}
	snap := &gateSnapshot{
		users:  cloneCounters(old.users),
		keys:   cloneCounters(old.keys),
		quotas: cloneCounters(old.quotas),
	}
	delete(snap.keys, keyID)
	delete(snap.quotas, keyID)
	g.store.Store(snap)
}

func cloneCounters(m map[int64]*atomic.Int64) map[int64]*atomic.Int64 {
	out := make(map[int64]*atomic.Int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// acquire 抢占门禁槽位（CAS 循环）。返回已 acquire 层级位掩码
// （1=user、2=key、3=两者；release 按位释放）。两步回滚（评审 I-3）：
// user 成功 key 失败 → 复原 user 计数再返回失败，防泄漏。
func (g *concurrencyGate) acquire(meta domain.KeyMeta) (int, bool) {
	snap := g.store.Load()
	level := 0
	if meta.UserMaxConc > 0 {
		if c, ok := snap.users[meta.UserID]; ok && c != nil {
			if !casInc(c, meta.UserMaxConc) {
				return 0, false // user 层超限 → 429（无任何计数占用）
			}
			level |= 1
		}
	}
	if meta.KeyMaxConc > 0 {
		if c, ok := snap.keys[meta.KeyID]; ok && c != nil {
			if !casInc(c, meta.KeyMaxConc) {
				if level&1 != 0 {
					if uc, ok := snap.users[meta.UserID]; ok {
						uc.Add(-1) // 回滚 user 计数
					}
				}
				return 0, false
			}
			level |= 2
		}
	}
	return level, true
}

func (g *concurrencyGate) release(meta domain.KeyMeta, level int) {
	if level == 0 {
		return
	}
	snap := g.store.Load()
	if level&1 != 0 {
		if c, ok := snap.users[meta.UserID]; ok && c != nil {
			c.Add(-1)
		}
	}
	if level&2 != 0 {
		if c, ok := snap.keys[meta.KeyID]; ok && c != nil {
			c.Add(-1)
		}
	}
}

// quotaExhausted 额度检查（纯读；无额度 key 短路 false）。
func (g *concurrencyGate) quotaExhausted(meta domain.KeyMeta) bool {
	if !meta.HasQuota {
		return false
	}
	snap := g.store.Load()
	if c, ok := snap.quotas[meta.KeyID]; ok && c != nil {
		return c.Load() >= meta.Quota
	}
	return meta.QuotaUsed >= meta.Quota // 无内存计数（新 key/竞态窗口）→ 快照值
}

// deductQuota 请求结束扣减（后扣模型；无额度 key 无条目 → no-op，恒 0）。
func (g *concurrencyGate) deductQuota(keyID, tokens int64) {
	if keyID <= 0 || tokens <= 0 {
		return
	}
	snap := g.store.Load()
	if c, ok := snap.quotas[keyID]; ok && c != nil {
		c.Add(tokens)
	}
}

// casInc CAS 循环自增：超过 max 返回 false（不占用）。
func casInc(c *atomic.Int64, max int) bool {
	for {
		cur := c.Load()
		if cur >= int64(max) {
			return false
		}
		if c.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}
