package sserelay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recorder+flush 的 writer：带 Flusher 的记录器。
// flushed 用 atomic：测试线程与 relay 的 flush 路径并发读写，-race 下需要同步。
type flusherRecorder struct {
	*httptest.ResponseRecorder
	flushed atomic.Int32
}

func (f *flusherRecorder) Flush() { f.flushed.Add(1); f.ResponseRecorder.Flush() }

func relayStream(w http.ResponseWriter, src string, cfg Config) error {
	return Relay(context.Background(), w, strings.NewReader(src), cfg)
}

func TestRelayPreservesRawBytes(t *testing.T) {
	src := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{}))
	require.Equal(t, src, rec.Body.String())
}

func TestRelayPreservesCRLFAndComments(t *testing.T) {
	src := ": comment line\r\nevent: ping\r\ndata: x\r\n\r\n"
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{}))
	require.Equal(t, src, rec.Body.String())
}

func TestRelayPreservesMultiLineData(t *testing.T) {
	src := "data: line1\ndata: line2\n\n"
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{}))
	require.Equal(t, src, rec.Body.String())
}

func TestRelayVeryLongFrame(t *testing.T) {
	long := strings.Repeat("x", 1<<20) // 1 MiB 单行，超过默认 4KiB bufio buffer
	src := "data: " + long + "\n\n"
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{}))
	require.Equal(t, src, rec.Body.String())
}

func TestRelayObserverReceivesTypedEvent(t *testing.T) {
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, "event: message_delta\ndata: {\"usage\":{\"output_tokens\":5}}\n\n", Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1)
	require.Equal(t, "message_delta", string(got[0].Event))
	require.Equal(t, `{"usage":{"output_tokens":5}}`, string(got[0].Data))
	require.Contains(t, string(got[0].Raw), "event: message_delta")
}

func TestRelayObserverReceivesDone(t *testing.T) {
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, "data: [DONE]\n\n", Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1)
	require.Equal(t, "", string(got[0].Event))
	require.Equal(t, "[DONE]", string(got[0].Data))
}

func TestRelayMultiLineDataMergedInObserver(t *testing.T) {
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, "data: a\ndata: b\n\n", Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1)
	require.Equal(t, "a\nb", string(got[0].Data)) // SSE 规范：多行 data 以 \n 连接
}

func TestEventNamePrefersEventField(t *testing.T) {
	e := Event{Event: []byte("response.completed"), Data: []byte(`{"type":"ignored"}`)}
	require.Equal(t, "response.completed", string(e.EventName()), "event: 字段优先，不依赖 data")
}

func TestEventNameInfersFromDataType(t *testing.T) {
	// 缺 event: 名的 data-only 帧（非规范上游，P3）：data JSON 的 type 与事件名同值
	e := Event{Data: []byte(`{"type":"response.output_text.delta","delta":"x"}`)}
	require.Equal(t, "response.output_text.delta", string(e.EventName()))
}

func TestEventNameEmptyWhenUninferrable(t *testing.T) {
	require.Empty(t, Event{Data: []byte("[DONE]")}.EventName(), "非 JSON 载荷")
	require.Empty(t, Event{Data: []byte(`{"no_type":1}`)}.EventName(), "JSON 但无 type 字段")
	require.Empty(t, Event{Data: nil}.EventName(), "空载荷")
}

func TestRelayFirstEventFlushesImmediately(t *testing.T) {
	fr := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	require.NoError(t, relayStream(fr, "data: {\"a\":1}\n\n", Config{FlushBytes: 4096, FlushInterval: time.Hour}))
	require.True(t, fr.Flushed, "首个事件必须触发 Flush")
	require.Equal(t, int32(1), fr.flushed.Load())
}

func TestRelayBatchesUntilThreshold(t *testing.T) {
	fr := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	// 3 个各 2056B 的事件，阈值 4096：首事件立即 flush（不重置累计，2056 仍计入阈值），
	// 第 2 个使累计 4112 >= 4096 触发 flush 并归零，第 3 个未达阈值，由流结束残余 flush
	var src strings.Builder
	for i := 0; i < 3; i++ {
		src.WriteString("data: ")
		src.WriteString(strings.Repeat("y", 2048))
		src.WriteString("\n\n")
	}
	require.NoError(t, relayStream(fr, src.String(), Config{FlushBytes: 4096, FlushInterval: time.Hour}))
	// 首事件 1 次 + 第 2 事件攒满阈值 1 次 + 流结束残余 1 次
	require.Equal(t, int32(3), fr.flushed.Load())
}

// stagedReader 依次返回 chunks，之后阻塞在 block 上：用于构造"流仍存活、
// 停在 Read 上、没有下一帧"的场景，让 timer 成为唯一可能的 flush 来源。
type stagedReader struct {
	chunks [][]byte
	block  chan struct{}
	idx    int
}

func (s *stagedReader) Read(p []byte) (int, error) {
	if s.idx < len(s.chunks) {
		n := copy(p, s.chunks[s.idx])
		s.idx++
		return n, nil
	}
	<-s.block
	return 0, io.EOF
}

func TestRelayTimerFlushesWithoutNextEvent(t *testing.T) {
	fr := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	src := &stagedReader{chunks: [][]byte{[]byte("data: x\n\n")}, block: make(chan struct{})}
	errCh := make(chan error, 1)
	go func() {
		errCh <- Relay(context.Background(), fr, src, Config{FlushBytes: 4096, FlushInterval: 20 * time.Millisecond})
	}()
	// 首事件立即 flush
	require.Eventually(t, func() bool { return fr.flushed.Load() >= 1 }, time.Second, time.Millisecond)
	// 源仍阻塞在 Read、relay 未退出：期间没有下一帧，2nd flush 只能来自 timer
	select {
	case err := <-errCh:
		t.Fatalf("relay 在源阻塞期间提前退出: %v", err)
	default:
	}
	require.Eventually(t, func() bool { return fr.flushed.Load() >= 2 }, 200*time.Millisecond, 5*time.Millisecond)
	// 解除阻塞，流正常结束
	close(src.block)
	require.NoError(t, <-errCh)
}

func TestRelayReaderErrorPropagates(t *testing.T) {
	rec := httptest.NewRecorder()
	rd := &errorReader{err: errors.New("boom")}
	err := Relay(context.Background(), rec, rd, Config{})
	require.Error(t, err)
}

type errorReader struct{ err error }

func (r *errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestRelayWriterErrorPropagates(t *testing.T) {
	wr := &errorWriter{}
	err := Relay(context.Background(), wr, strings.NewReader("data: x\n\n"), Config{})
	require.Error(t, err)
}

type errorWriter struct{}

func (w *errorWriter) Header() http.Header         { return http.Header{} }
func (w *errorWriter) Write(p []byte) (int, error) { return 0, errors.New("client gone") }
func (w *errorWriter) WriteHeader(int)             {}

func TestRelayContextCancelStops(t *testing.T) {
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消
	rd := &blockingReader{ch: make(chan struct{})}
	errCh := make(chan error, 1)
	go func() { errCh <- Relay(ctx, rec, rd, Config{}) }()
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("ctx cancel 未停止 relay")
	}
}

type blockingReader struct{ ch chan struct{} }

func (r *blockingReader) Read(p []byte) (int, error) { <-r.ch; return 0, io.EOF }

// plainWriter 只实现 http.ResponseWriter 的 Header/Write/WriteHeader，
// 刻意不提供 Flush 方法，用于覆盖 dst 无 Flusher 的路径。
type plainWriter struct{ buf bytes.Buffer }

func (w *plainWriter) Header() http.Header         { return http.Header{} }
func (w *plainWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *plainWriter) WriteHeader(int)             {}

func TestRelayNoFlusherStillWrites(t *testing.T) {
	wr := &plainWriter{} // 无 Flusher：flush 路径跳过 fl.Flush，但字节仍完整写出
	require.NoError(t, relayStream(wr, "data: x\n\n", Config{}))
	require.Equal(t, "data: x\n\n", wr.buf.String())
}

func TestRelayTimerGoroutineExits(t *testing.T) {
	// 大量短流：每流 timer 必须退出，-race 下无泄漏/竞争
	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		require.NoError(t, relayStream(rec, "data: x\n\n", Config{FlushInterval: time.Millisecond}))
	}
}

func TestRelayConcurrentStreams(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			require.NoError(t, relayStream(rec, "data: x\n\ndata: y\n\n", Config{FlushBytes: 1024, FlushInterval: time.Millisecond}))
		}()
	}
	wg.Wait()
}
