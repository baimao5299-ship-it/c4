package proxy

import (
	"sync"
	"time"
)

// fixedWindowLimiter 每分组 key 的固定窗口计数限流（规格 §10.6，默认关闭）。
type fixedWindowLimiter struct {
	rpm int
	mu  sync.Mutex
	win map[int64]windowState
}

type windowState struct {
	start time.Time
	count int64
}

func newFixedWindowLimiter(rpm int) *fixedWindowLimiter {
	return &fixedWindowLimiter{rpm: rpm, win: make(map[int64]windowState)}
}

func (l *fixedWindowLimiter) Allow(groupID int64, now time.Time) bool {
	if l.rpm <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.win[groupID]
	if !ok || now.Sub(st.start) >= time.Minute {
		l.win[groupID] = windowState{start: now, count: 1}
		return true
	}
	st.count++
	l.win[groupID] = st
	return st.count <= int64(l.rpm)
}
