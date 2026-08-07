package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/pkg/cryptox"
	"go-proxy-mini/pkg/logx"
)

// KeyLoader 由 repository.KeyRepo 实现（keys 独立表鉴权快照）。
type KeyLoader interface {
	LoadKeys(ctx context.Context) (map[string]domain.KeyMeta, error)
}

// UserStatusLoader 由 repository.UserRepo 实现（RequireJWT 用户状态快照）。
type UserStatusLoader interface {
	LoadUsers(ctx context.Context) (map[int64]domain.UserStatus, error)
}

// Auth 鉴权快照：key_hash → KeyMeta（含归属用户门禁字段）+ 用户状态表 +
// 两级并发/额度内存计数（gate）。热路径零 DB、零 per-request 锁（RWMutex
// 读多写少，规格 §10.3）。用户状态变更（禁用/并发/额度调整）走 invalidate
// 回调 → Reload 全量刷新（评审 I-2），JWT 15min 短时效兜底。
type Auth struct {
	loader KeyLoader
	users  UserStatusLoader
	log    *logx.Logger
	mu     sync.RWMutex
	keys   map[string]domain.KeyMeta
	states map[int64]domain.UserStatus
	gate   *concurrencyGate
}

func NewAuth(loader KeyLoader, users UserStatusLoader, log *logx.Logger) *Auth {
	a := &Auth{
		loader: loader,
		users:  users,
		log:    log,
		keys:   make(map[string]domain.KeyMeta),
		states: make(map[int64]domain.UserStatus),
		gate:   newConcurrencyGate(),
	}
	_ = a.Reload(context.Background())
	return a
}

// Reload 全量刷新鉴权快照（启动/定时/用户变更 invalidate）：
// keys 元数据 + 用户状态 + 门禁计数器（在途值跨 reload 继承）。
func (a *Auth) Reload(ctx context.Context) error {
	m, err := a.loader.LoadKeys(ctx)
	if err != nil {
		return err
	}
	u, err := a.users.LoadUsers(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.keys = m
	a.states = u
	a.mu.Unlock()
	a.gate.reload(m)
	return nil
}

// Upsert 增量刷新单个 key（key 创建/轮换/更新后调用；门禁计数器同步）。
func (a *Auth) Upsert(hash string, meta domain.KeyMeta) {
	a.mu.Lock()
	a.keys[hash] = meta
	a.mu.Unlock()
	a.gate.upsert(meta)
}

func (a *Auth) Delete(hash string) {
	a.mu.Lock()
	meta, ok := a.keys[hash]
	delete(a.keys, hash)
	a.mu.Unlock()
	if ok {
		a.gate.delete(meta.KeyID)
	}
}

// UserStatus 用户状态快照（RequireJWT 校验用；用户变更走 invalidate → Reload，
// 不用 DB 直查）。
func (a *Auth) UserStatus(userID int64) (domain.UserStatus, bool) {
	a.mu.RLock()
	s, ok := a.states[userID]
	a.mu.RUnlock()
	return s, ok
}

// Authenticate 解析网关 key 并返回 KeyMeta。兼容两种客户端口径：
// OpenAI 客户端发 Authorization: Bearer；Anthropic 官方 SDK / Claude Code
// 发 x-api-key 头。两者同时提供时以 Authorization 为准。
// key 或归属用户被禁用 → 快照直接拒绝（401，即时失效）。
func (a *Auth) Authenticate(r *http.Request) (domain.KeyMeta, bool) {
	raw := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		raw = strings.TrimPrefix(h, "Bearer ")
	} else if h := r.Header.Get("x-api-key"); h != "" {
		raw = h
	}
	if raw == "" {
		return domain.KeyMeta{}, false
	}
	hash := cryptox.HashKey(raw)
	a.mu.RLock()
	meta, ok := a.keys[hash]
	a.mu.RUnlock()
	if !ok {
		return domain.KeyMeta{}, false
	}
	if meta.KeyStatus != domain.KeyStatusActive || meta.UserStatus != domain.UserStatusActive {
		return domain.KeyMeta{}, false
	}
	return meta, true
}

// --- 门禁（内存原子；热路径零 DB 零锁） ---

// Acquire 两级并发门禁：user → key 依次 CAS 抢占；key 失败回滚 user 计数
// （评审 I-3：防泄漏）。返回已 acquire 层级位掩码（release 仅释放已 acquire
// 层级）。未设置上限（max=0）或计数器缺失（跨 reload 竞态窗口）→ 该层跳过。
func (a *Auth) Acquire(meta domain.KeyMeta) (int, bool) {
	return a.gate.acquire(meta)
}

// Release 释放并发计数（仅释放 acquire 返回的层级；跨 reload 命中新快照的
// 继承计数，与 scheduler Release 同语义）。
func (a *Auth) Release(meta domain.KeyMeta, level int) {
	a.gate.release(meta, level)
}

// QuotaExhausted 额度检查（纯读快照/内存计数，无计数副作用——评审提醒①：
// 检查在并发 acquire 之前；未设置额度 key 短路零成本）。
func (a *Auth) QuotaExhausted(meta domain.KeyMeta) bool {
	return a.gate.quotaExhausted(meta)
}

// DeductQuota 请求结束扣减（后扣模型；usage 已知；无额度 key 无计数器 → no-op）。
func (a *Auth) DeductQuota(keyID, tokens int64) {
	a.gate.deductQuota(keyID, tokens)
}
