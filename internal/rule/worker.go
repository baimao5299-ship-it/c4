package rule

import (
	"context"
	"fmt"
	"time"

	"go-proxy-mini/pkg/logx"
)

// cleanupInterval 过期账号清理周期（窗口计数防 map 泄漏）。
const cleanupInterval = time.Minute

// Name 满足 worker.Worker 契约（Global Constraints #5）。
func (e *RuleEngine) Name() string { return "rule-engine" }

// Start 启动事件消费循环（含周期性窗口清理）；重复 Start 返回错误（幂等）。
func (e *RuleEngine) Start(ctx context.Context) error {
	if !e.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("rule-engine: already started")
	}
	go e.loop(ctx)
	return nil
}

// loop 消费循环：ctx 取消退出；scheduler 投递不依赖启动态（事件先入队）。
func (e *RuleEngine) loop(ctx context.Context) {
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.ch:
			e.HandleEvent(ctx, ev)
		case <-t.C:
			e.wm.cleanup(e.timeNow())
		}
	}
}

// Flush 同步排空队列：处理完当前队列中的全部事件后返回（测试与优雅关闭用）。
// 幂等，未 Start 时也可安全排空。
func (e *RuleEngine) Flush(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.ch:
			e.HandleEvent(ctx, ev)
		default:
			return
		}
	}
}

// Close 排空剩余事件（限时，复用 scheduler.Close 模式）；幂等，
// 未 Start 时也可安全排空。循环本身随 Start 的 ctx 取消而退出。
func (e *RuleEngine) Close(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case ev := <-e.ch:
				e.HandleEvent(ctx, ev)
			default:
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		if e.log != nil {
			e.log.Warn("rule-engine close timeout, dropping queued events")
		}
	}
	return nil
}

// Enqueue 投递事件：有界 channel，满则丢弃（dropped 原子计数 + 告警日志）。
func (e *RuleEngine) Enqueue(ev Event) {
	select {
	case e.ch <- ev:
	default:
		e.dropped.Add(1)
		if e.log != nil {
			e.log.Warn("rule-engine event queue full, dropping event",
				logx.Int64("account_id", ev.AccountID),
				logx.String("kind", ev.Kind.String()),
			)
		}
	}
}
