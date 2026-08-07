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
// Raw/Event/Data 均指向 relay 内部复用的缓冲，仅在本次 Observer 回调期间有效；
// 消费方不得跨帧保留这些切片（下一帧会复用同一批缓冲）。
type Event struct {
	Raw   []byte // 完整原始帧（含结尾空行）
	Event []byte // event: 字段值；data-only 帧为空
	Data  []byte // 合并后的 data: payload（多行以 \n 连接）
}

// Observer 在帧原样写出后调用；不得阻塞 relay，不得修改已写出的字节。
// 回调参数 Event 的各切片仅在回调内有效（见 Event 注释），不得跨帧保留。
type Observer func(Event)

type Config struct {
	FlushBytes    int           // 缓冲达到该值立即 flush；0 时默认 4096
	FlushInterval time.Duration // 从 relay 启动起以固定间隔触发 timer flush（仅 pending > 0 时实际 flush）；0 时默认 1ms
	Observer      Observer
}

type relay struct {
	ctx context.Context
	bw  *bufio.Writer
	fl  http.Flusher
	cfg Config

	mu       sync.Mutex // 保护 bw/pending/timer
	pending  int        // 累计写入字节；阈值/timer/结束残余 flush 后归零（首事件 latency flush 不归零，其字节继续计入阈值）
	lastTick time.Time

	timer     *time.Timer
	timerArmed bool // 按需武装：仅当存在待 flush 数据时 timer 才在跑（瞬时短流零 timer 开销）
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
	// 读循环退出后（timer 已停、goroutine 已退出）再 flush 残余并归还 writer；
	// 仅实际仍有缓冲字节时才 flush（首事件已 flush 后无残余，不产生多余 Flush）
	r.mu.Lock()
	if r.bw.Buffered() > 0 {
		_ = r.flushLocked()
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
				continue
			}
			if len(line) == 1 && line[0] == '\n' {
				// 空行（LF）
				frame.Write(line)
				if err := flushFrame(); err != nil {
					return err
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
		// 首事件立即 flush，保证首字节延迟；不重置 pending——首事件字节仍计入
		// 阈值，后续小事件可叠加触发一次批量 flush（区分于阈值 flush 的归零语义）
		if err := r.flushNoResetLocked(); err != nil {
			return err
		}
	}
	if r.pending >= r.cfg.FlushBytes {
		return r.flushLocked()
	}
	// 按需武装 flush timer：仅当有待 flush 数据时 timer 才运行。**瞬时短流
	// （上游秒回、首事件已 flush）绝无 timer 开销**——否则每流一个 1ms 周期
	// timer + goroutine，万级并发流 = 每秒千万次 timer 唤醒，runtime 计时器
	// 堆 + 锁打满 CPU（50k 并发上机实测：timers.run 26% + timer 锁 16%，
	// 吞吐从 11.9k/s 崩到 3k/s 的死亡螺旋；短流 ~1ms 结束所以旧行为不炸）。
	r.armFlushTimerLocked()
	return nil
}

// armFlushTimerLocked 武装 flush timer（一次写入只武装一次；触发后由 timer
// goroutine 置回未武装，下次写入按需重新武装）。
func (r *relay) armFlushTimerLocked() {
	if !r.timerArmed {
		r.timerArmed = true
		r.timer.Reset(r.cfg.FlushInterval)
	}
}

func (r *relay) startFlushTimer() {
	r.timer = time.NewTimer(time.Hour)
	r.timer.Stop() // 初始未武装（见 armFlushTimerLocked 注释）
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
				_ = r.flushLocked() // 触发时必有待 flush 数据（武装前提），pending 归零
				r.timerArmed = false
				r.mu.Unlock()
			}
		}
	}()
}

func (r *relay) stopFlushTimer() {
	r.timer.Stop()
	close(r.stopFlush) // 唤醒可能阻塞在 select 上的 timer goroutine
	<-r.timerDone      // 等 timer goroutine 退出后才允许释放 writer
}

// flushLocked 批量 flush（阈值 / timer / 结束残余触发）：pending > 0 时执行
// bw.Flush + fl.Flush，并把 pending 归零，使事件重新累积批量（spec 规则 2/3）。
// 不检查 bw.Buffered()：>= 8192B 的帧走 bufio 直写路径时缓冲为空但确实有数据
// 待 flush，bw.Flush 对空缓冲是廉价 no-op，随后仍需 fl.Flush 把数据推给对端。
// 错误返回给写路径上报；timer goroutine 与退出路径忽略（客户端断开不可恢复）。
func (r *relay) flushLocked() error {
	if r.pending <= 0 {
		return nil
	}
	if err := r.bw.Flush(); err != nil {
		return err
	}
	if r.fl != nil {
		r.fl.Flush()
	}
	r.pending = 0
	return nil
}

// flushNoResetLocked 只 flush 不重置 pending：首事件 latency flush 专用。
func (r *relay) flushNoResetLocked() error {
	if err := r.bw.Flush(); err != nil {
		return err
	}
	if r.fl != nil {
		r.fl.Flush()
	}
	return nil
}
