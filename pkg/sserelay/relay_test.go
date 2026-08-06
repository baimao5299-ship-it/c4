package sserelay

import (
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

func TestRelayFirstEventFlushesImmediately(t *testing.T) {
	fr := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	require.NoError(t, relayStream(fr, "data: {\"a\":1}\n\n", Config{FlushBytes: 4096, FlushInterval: time.Hour}))
	require.True(t, fr.Flushed, "首个事件必须触发 Flush")
	require.Equal(t, int32(1), fr.flushed.Load())
}

func TestRelayBatchesUntilThreshold(t *testing.T) {
	fr := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	// 3 个各 2KB 的事件：第 1 个立即 flush，第 2 个攒到 4KB 后 flush，第 3 个等 timer/结束 flush
	var src strings.Builder
	for i := 0; i < 3; i++ {
		src.WriteString("data: ")
		src.WriteString(strings.Repeat("y", 2048))
		src.WriteString("\n\n")
	}
	require.NoError(t, relayStream(fr, src.String(), Config{FlushBytes: 4096, FlushInterval: time.Hour}))
	// 首事件 1 次 + 第 2 事件攒满 1 次 + 流结束 flush 1 次
	require.Equal(t, int32(3), fr.flushed.Load())
}

func TestRelayTimerFlushesWithoutNextEvent(t *testing.T) {
	fr := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Relay(ctx, fr, strings.NewReader("data: x\n\n"), Config{FlushBytes: 4096, FlushInterval: 20 * time.Millisecond})
	}()
	// 首事件已 flush；等 100ms（期间无下一帧）验证 timer 继续 flush
	require.Eventually(t, func() bool { return fr.flushed.Load() >= 2 }, time.Second, 10*time.Millisecond)
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

func TestRelayNoFlusherStillWrites(t *testing.T) {
	rec := httptest.NewRecorder() // 无 Flusher
	require.NoError(t, relayStream(rec, "data: x\n\n", Config{}))
	require.Equal(t, "data: x\n\n", rec.Body.String())
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
