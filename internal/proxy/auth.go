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

// Authenticate 解析 Bearer key 并返回 groupID。
func (a *Auth) Authenticate(r *http.Request) (int64, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return 0, false
	}
	raw := strings.TrimPrefix(h, "Bearer ")
	if raw == "" {
		return 0, false
	}
	hash := cryptox.HashKey(raw)
	a.mu.RLock()
	gid, ok := a.keys[hash]
	a.mu.RUnlock()
	return gid, ok
}
