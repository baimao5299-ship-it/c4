package scheduler

import (
	"sync/atomic"
	"time"

	"go-proxy-mini/internal/domain"
)

// errRateScale 是错误率 EWMA 的定点缩放（0..1e6）。
const errRateScale = 1_000_000

type accState struct {
	status        domain.AccountStatus
	cooldownUntil *time.Time
	errCount      int
	lastError     *string
	lastUsedAt    *time.Time
}

type accountSnapshot struct {
	acc         domain.Account
	tpl         *domain.Template
	concurrency atomic.Int64
	errRate     atomic.Uint64 // 定点
	state       atomic.Pointer[accState]
}

func (a *accountSnapshot) statePtr() *accState {
	st := a.state.Load()
	if st == nil {
		st = &accState{status: domain.StatusActive}
		a.state.Store(st)
	}
	return st
}

func (a *accountSnapshot) score() float64 {
	rate := float64(a.errRate.Load()) / errRateScale
	return float64(a.acc.Weight) * (1 - rate)
}

type groupSnapshot struct {
	accounts []*accountSnapshot
}

// snapshotStore 整体换入换出（atomic.Value），重建不阻塞请求路径。
type snapshotStore struct {
	groups atomic.Value // map[int64]*groupSnapshot
	byID   atomic.Value // map[int64]*accountSnapshot
}

func (s *snapshotStore) store(groups map[int64]*groupSnapshot, byID map[int64]*accountSnapshot) {
	s.groups.Store(groups)
	s.byID.Store(byID)
}
