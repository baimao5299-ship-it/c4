package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"go-proxy-mini/pkg/cryptox"
	"go-proxy-mini/pkg/logx"
)

// KeyLoader 由 repository.GroupRepo 实现。
type KeyLoader interface {
	LoadGroupKeys(ctx context.Context) (map[string]int64, error)
}

// Auth 分组 key 鉴权：内存哈希表（RWMutex，读多写少，规格 §10.3）。
type Auth struct {
	loader KeyLoader
	log    *logx.Logger
	mu     sync.RWMutex
	keys   map[string]int64 // key_hash -> groupID
}

func NewAuth(loader KeyLoader, log *logx.Logger) *Auth {
	a := &Auth{loader: loader, log: log, keys: make(map[string]int64)}
	_ = a.Reload(context.Background())
	return a
}

func (a *Auth) Reload(ctx context.Context) error {
	m, err := a.loader.LoadGroupKeys(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.keys = m
	a.mu.Unlock()
	return nil
}

func (a *Auth) Upsert(hash string, groupID int64) {
	a.mu.Lock()
	a.keys[hash] = groupID
	a.mu.Unlock()
}

func (a *Auth) Delete(hash string) {
	a.mu.Lock()
	delete(a.keys, hash)
	a.mu.Unlock()
}

// Authenticate 解析网关 key 并返回 groupID。兼容两种客户端口径：
// OpenAI 客户端发 Authorization: Bearer；Anthropic 官方 SDK / Claude Code
// 发 x-api-key 头。两者同时提供时以 Authorization 为准。
func (a *Auth) Authenticate(r *http.Request) (int64, bool) {
	raw := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		raw = strings.TrimPrefix(h, "Bearer ")
	} else if h := r.Header.Get("x-api-key"); h != "" {
		raw = h
	}
	if raw == "" {
		return 0, false
	}
	hash := cryptox.HashKey(raw)
	a.mu.RLock()
	gid, ok := a.keys[hash]
	a.mu.RUnlock()
	return gid, ok
}
