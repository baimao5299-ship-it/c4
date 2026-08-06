// Package sserelay 提供原始字节级 SSE relay：从 io.Reader 增量读取 SSE 帧，
// 原样转发给 http.ResponseWriter，自适应批量 Flush，并以 Observer 旁路暴露
// 事件信息（仅用于 usage 提取，不参与转发决策）。
package sserelay

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// Event 是一次 SSE 事件的旁路视图。
type Event struct {
	Raw   []byte // 完整原始帧（含结尾空行）
	Event []byte // event: 字段值；data-only 帧为空
	Data  []byte // 合并后的 data: payload（多行以 \n 连接）
}

// Observer 在帧原样写出后调用；不得阻塞 relay，不得修改已写出的字节。
type Observer func(Event)

type Config struct {
	FlushBytes    int           // 缓冲达到该值立即 flush；0 时默认 4096
	FlushInterval time.Duration // 距上次 flush 达到该值时 timer flush；0 时默认 1ms
	Observer      Observer
}

// keepAliveMaxInterval 界定"交互式" FlushInterval：不超过该值时，流结束前补一次
// keep-alive flush，让对端尽快感知流结束；1h 级别的 interval 下这是无谓开销，跳过。
const keepAliveMaxInterval = 100 * time.Millisecond

type relay struct {
	ctx context.Context
	bw  *bufio.Writer
	fl  http.Flusher
	cfg Config

	mu       sync.Mutex // 保护 bw/pending/timer
	pending  int        // 已写入的累计字节（含首事件；阈值判定用，flush 不重置）
	lastTick time.Time

	timer     *time.Timer
	stopFlush chan struct{} // 关闭后 timer goroutine 退出
	timerDone chan struct{}
}

// Relay 把 src 的 SSE 流原样转发到 dst。流结束 = EOF / 读错误 / ctx 取消。
func Relay(ctx context.Context, dst http.ResponseWriter, src io.Reader, cfg Config) error {
	if cfg.FlushBytes <= 0 {
		cfg.FlushBytes = 4096
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Millisecond
	}
	r := &relay{
		ctx: ctx, cfg: cfg,
		bw:        bufio.NewWriterSize(dst, 8192),
		stopFlush: make(chan struct{}),
		timerDone: make(chan struct{}),
	}
	r.fl, _ = dst.(http.Flusher)
	r.startFlushTimer()

	err := r.run(&ctxReader{ctx: ctx, r: src})
	r.stopFlushTimer()
	// 读循环退出后（timer 已停、goroutine 已退出）再 flush 残余并归还 writer
	r.mu.Lock()
	_ = r.flushLocked()
	if err == nil && r.cfg.FlushInterval <= keepAliveMaxInterval {
		// 短 interval 的交互式流：退出前补一次 keep-alive flush
		r.keepAliveFlushLocked()
	}
	r.mu.Unlock()
	return err
}

// ctxReader 在每次 Read 前检查 ctx 是否已取消，避免已取消的 ctx 下阻塞在 src 上。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}
	return c.r.Read(p)
}

func (r *relay) run(src io.Reader) error {
	br := bufio.NewReaderSize(src, 8192)
	var (
		frame bytes.Buffer // 当前帧原始字节
		data  []byte       // 当前帧 data payload（合并）
		event []byte       // 当前帧 event 字段
	)
	flushFrame := func() error {
		if err := r.write(frame.Bytes()); err != nil {
			return err
		}
		if r.cfg.Observer != nil {
			r.cfg.Observer(Event{Raw: frame.Bytes(), Event: event, Data: data})
		}
		frame.Reset()
		data = data[:0]
		event = event[:0]
		return nil
	}
	for {
		// 空行 = 帧结束
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			if len(line) == 2 && line[0] == '\r' && line[1] == '\n' {
				// 空行（CRLF）
				frame.Write(line)
				if err := flushFrame(); err != nil {
					return err
				}
				if err != nil {
					return r.normalize(err)
				}
				continue
			}
			if len(line) == 1 && line[0] == '\n' {
				// 空行（LF）
				frame.Write(line)
				if err := flushFrame(); err != nil {
					return err
				}
				if err != nil {
					return r.normalize(err)
				}
				continue
			}
			frame.Write(line)
			field, value := splitField(line)
			switch string(field) {
			case "event":
				event = append(event[:0], value...)
			case "data":
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, value...)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err == bufio.ErrBufferFull {
			continue // 长行：继续累积该行剩余部分
		}
		if err != nil {
			return r.normalize(err)
		}
		if err := r.checkCancel(); err != nil {
			return err
		}
	}
}

// splitField 解析 "name: value" 行；无冒号或注释行返回 ("", nil)。
func splitField(line []byte) ([]byte, []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return nil, nil
	}
	name := line[:i]
	if len(name) == 0 || name[0] == ':' { // 注释行
		return nil, nil
	}
	val := line[i+1:]
	if len(val) > 0 && val[0] == ' ' {
		val = val[1:]
	}
	// 去掉行尾 \n / \r\n
	for len(val) > 0 && (val[len(val)-1] == '\n' || val[len(val)-1] == '\r') {
		val = val[:len(val)-1]
	}
	return name, val
}

func (r *relay) normalize(err error) error {
	if err == context.Canceled || r.ctx.Err() != nil {
		return context.Canceled
	}
	return err
}

func (r *relay) checkCancel() error {
	select {
	case <-r.ctx.Done():
		return context.Canceled
	default:
		return nil
	}
}

func (r *relay) write(p []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.bw.Write(p); err != nil {
		return err
	}
	r.pending += len(p)
	first := r.lastTick.IsZero()
	r.lastTick = time.Now()
	if first {
		// 首事件立即 flush，保证首字节延迟
		return r.flushLocked()
	}
	if r.pending >= r.cfg.FlushBytes {
		return r.flushLocked()
	}
	return nil
}

func (r *relay) startFlushTimer() {
	r.timer = time.NewTimer(r.cfg.FlushInterval)
	go func() {
		defer close(r.timerDone)
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-r.stopFlush:
				return
			case <-r.timer.C:
				r.mu.Lock()
				_ = r.flushLocked()
				r.mu.Unlock()
				r.timer.Reset(r.cfg.FlushInterval)
			}
		}
	}()
}

func (r *relay) stopFlushTimer() {
	r.timer.Stop()
	close(r.stopFlush) // 唤醒可能阻塞在 select 上的 timer goroutine
	<-r.timerDone      // 等 timer goroutine 退出后才允许释放 writer
}

// flushLocked 把缓冲的字节刷到 dst；无缓冲时直接返回（不产生空 Flush）。
// 错误返回给写路径上报；timer goroutine 与退出路径忽略它（客户端断开不可恢复）。
func (r *relay) flushLocked() error {
	if r.bw.Buffered() == 0 {
		return nil
	}
	if err := r.bw.Flush(); err != nil {
		return err
	}
	if r.fl != nil {
		r.fl.Flush()
	}
	return nil
}

// keepAliveFlushLocked 无条件触发一次 Flush（即使无缓冲数据）；net/http 下空 Flush 为 no-op。
func (r *relay) keepAliveFlushLocked() {
	if r.fl != nil {
		r.fl.Flush()
	}
}
