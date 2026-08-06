package scheduler

import (
	"math/rand/v2"
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

// routeKey 是预生成调度路径的桶键；model == "" 表示默认回退桶
// （请求模型不在任何模板可服务集合内时，行为等价于默认格式的 tier2）。
type routeKey struct {
	format domain.RequestFormat
	model  string
}

// weightedSeq 是按权重预生成的循环段（GCD 归一化 + 一次 shuffle），
// 请求时以原子游标取模取用。重建时生成，Select 热路径零计算。
type weightedSeq struct {
	seq    []*accountSnapshot
	cursor atomic.Uint64 //nolint:unused // Task 2（Select 迁移）将消费该游标
}

// route 是 (format, model) 桶：模型命中（Serves）走 tier1，未命中走 tier2。
type route struct {
	tier1 *weightedSeq
	tier2 *weightedSeq
}

// maxSeqLen 是归一化序列长度上限：超出按比例缩放截断（防极端权重比）。
const maxSeqLen = 4096

func newWeightedSeq(pool []*accountSnapshot) *weightedSeq {
	g := 0
	for _, a := range pool {
		g = gcdInt(g, a.acc.Weight)
	}
	if g <= 0 {
		g = 1
	}
	var total int
	for _, a := range pool {
		total += a.acc.Weight / g
	}
	// 超长缩放：ceil 除法降权，每个账号至少保留 1 次。语义：序列长度 ≈
	// max(账号数, maxSeqLen) 量级——上限防极端权重比（9999:1 → 10000 长）的
	// 膨胀，账号数本身超过上限时不硬截（O(账号数) 可接受）。
	scale := 1
	if total > maxSeqLen {
		scale = (total + maxSeqLen - 1) / maxSeqLen // ceil
	}
	ws := &weightedSeq{}
	ws.seq = make([]*accountSnapshot, 0, total/scale+len(pool))
	for _, a := range pool {
		n := a.acc.Weight / g / scale
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			ws.seq = append(ws.seq, a)
		}
	}
	rand.Shuffle(len(ws.seq), func(i, j int) { ws.seq[i], ws.seq[j] = ws.seq[j], ws.seq[i] })
	return ws
}

func gcdInt(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

type groupSnapshot struct {
	accounts []*accountSnapshot
	routes   map[routeKey]*route
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
