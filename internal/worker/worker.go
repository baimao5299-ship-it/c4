// Package worker 提供统一后台任务抽象（Global Constraints #5）：顺序启动、
// 反向排空、panic 捕获（进程不崩）。
package worker

import (
	"context"
	"fmt"
	"sync"

	"go-proxy-mini/pkg/logx"
)

// Worker 是后台任务统一契约：Start 非阻塞启动内部 goroutine，Close 排空/释放。
// 两者必须幂等；Close 在未 Start 时也必须安全。
type Worker interface {
	Name() string
	Start(ctx context.Context) error
	Close(ctx context.Context) error
}

// Manager 统一装配后台任务：顺序启动、反向排空、panic 捕获（进程不崩）。
type Manager struct {
	log     *logx.Logger
	mu      sync.Mutex
	workers []Worker
	started bool
}

func New(log *logx.Logger) *Manager { return &Manager{log: log} }

func (m *Manager) Register(ws ...Worker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workers = append(m.workers, ws...)
}

// StartAll 按注册顺序启动；任一失败则已启动者反向 Close 后返回错误。
func (m *Manager) StartAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return fmt.Errorf("worker manager: already started")
	}
	m.started = true
	var started []Worker
	for _, w := range m.workers {
		if err := w.Start(ctx); err != nil {
			for i := len(started) - 1; i >= 0; i-- {
				if cerr := started[i].Close(ctx); cerr != nil && m.log != nil {
					m.log.Warn("worker close failed on start rollback", logx.String("worker", started[i].Name()), logx.Error(cerr))
				}
			}
			return fmt.Errorf("worker %s start: %w", w.Name(), err)
		}
		started = append(started, w)
	}
	return nil
}

// Shutdown 反向顺序 Close（后注册先关），收集并返回首个错误。
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for i := len(m.workers) - 1; i >= 0; i-- {
		w := m.workers[i]
		if err := w.Close(ctx); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("worker %s close: %w", w.Name(), err)
			}
			if m.log != nil {
				m.log.Warn("worker close failed", logx.String("worker", w.Name()), logx.Error(err))
			}
		}
	}
	return firstErr
}

// Go 托管命名 goroutine：panic 捕获 + Warn，进程不崩。
func (m *Manager) Go(ctx context.Context, name string, fn func(ctx context.Context)) {
	go func() {
		defer func() {
			if r := recover(); r != nil && m.log != nil {
				m.log.Warn("worker goroutine panicked", logx.String("worker", name), logx.Any("panic", r))
			}
		}()
		fn(ctx)
	}()
}
