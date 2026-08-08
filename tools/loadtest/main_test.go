package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---- 采样直方图（无锁固定桶数组）行为固定 ----

func TestP99LatencyUsesDedicatedHistogram(t *testing.T) {
	m := &metrics{}
	m.latencySamples[1].Store(2)
	m.latencySamples[5].Store(1)
	require.Equal(t, int64(10), p99Latency(m))
}

func TestP99Empty(t *testing.T) {
	m := &metrics{}
	require.Equal(t, int64(-1), p99(m))
	require.Equal(t, int64(-1), p99Latency(m))
}

func TestP99BucketsCrossing(t *testing.T) {
	// 100 个 500ms + 100 个 900ms：99% 分位 = 900ms 桶（90*10）
	m := &metrics{}
	for i := 0; i < 100; i++ {
		storeSample(m, 500)
		storeSample(m, 900)
	}
	require.Equal(t, int64(900), p99(m))
}

func TestP99BoundaryBucketClampsOversize(t *testing.T) {
	// ≥10.24s 延迟进边界桶 1023，p99 返回 1023*10 = 10230（超界不精确，结构兼容）
	m := &metrics{}
	for i := 0; i < 100; i++ {
		storeSample(m, 20000) // 20s，超出 0-10.24s 覆盖区间
	}
	require.Equal(t, int64(10230), p99(m))
}

// ---- SSE 行读取（零分配 drainSSE）行为固定 ----

func TestDrainSSEStopsAtDone(t *testing.T) {
	src := "data: a\n\ndata: b\n\ndata: [DONE]\n\ndata: 不应读到\n"
	require.NoError(t, drainSSE(bufio.NewReader(bytes.NewBufferString(src))))
}

func TestDrainSSEReadsToEOFWithoutDone(t *testing.T) {
	src := "data: a\n\ndata: b\n\n"
	require.True(t, errors.Is(drainSSE(bufio.NewReader(bytes.NewBufferString(src))), io.EOF))
}

func TestDrainSSELongLine(t *testing.T) {
	// 行超过 bufio 默认缓冲（4096B），ReadSlice 分段 + ErrBufferFull 累积
	long := bytes.Repeat([]byte("x"), 8000)
	src := append(long, []byte("\ndata: [DONE]\n\n")...)
	require.NoError(t, drainSSE(bufio.NewReader(bytes.NewBuffer(src))))
}

func TestDrainSSEDoneInsideLongLine(t *testing.T) {
	line := append(bytes.Repeat([]byte("x"), 5000), []byte("[DONE] 后缀")...)
	src := append(line, []byte("\n\n")...)
	require.NoError(t, drainSSE(bufio.NewReader(bytes.NewBuffer(src))))
}

func TestDrainSSEFinalLineWithoutNewline(t *testing.T) {
	// 流在 [DONE] 行后无 \n 直接 EOF：仍按 [DONE] 终止
	src := "data: x\ndata: [DONE]"
	require.NoError(t, drainSSE(bufio.NewReader(bytes.NewBufferString(src))))
}

// ---- 请求构造（模板格式）行为固定 ----

func TestRequestTemplateMatchesNewRequest(t *testing.T) {
	buildReqTemplate()
	got := newRequestFromTemplate("gk-test")
	want := newLoadtestRequest(*addr, "gk-test", *mode)
	require.Equal(t, want.Method, got.Method)
	require.Equal(t, want.URL.String(), got.URL.String())
	require.Equal(t, "application/json", got.Header.Get("Content-Type"))
	require.Equal(t, "Bearer gk-test", got.Header.Get("Authorization"))
	require.Equal(t, int64(len(tmplBody)), got.ContentLength)
	b, _ := io.ReadAll(got.Body)
	require.JSONEq(t, `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`, string(b))
}

// ---- [DONE] 后响应体尾部排空（连接复用）行为固定 ----

func TestDrainTailReadsToEOF(t *testing.T) {
	// chunked 尾部（空行 + 结束块）立刻 EOF：排空成功，连接可复用
	body := io.NopCloser(bytes.NewBufferString("\n0\r\n\r\n"))
	require.NoError(t, drainTail(body))
}

func TestDrainTailBoundedWhenNoEOF(t *testing.T) {
	// 服务端不结束流：50ms 超时放弃，绝不挂死
	start := time.Now()
	err := drainTail(newBlockingBody())
	require.ErrorIs(t, err, errDrainTimeout)
	require.Less(t, time.Since(start), 2*time.Second)
}

// blockingBody 永不返回数据的响应体；Close 解除阻塞（模拟服务端不主动结束的流）。
type blockingBody struct{ done chan struct{} }

func newBlockingBody() *blockingBody { return &blockingBody{done: make(chan struct{})} }

func (b *blockingBody) Read([]byte) (int, error) { <-b.done; return 0, io.EOF }

func (b *blockingBody) Close() error { close(b.done); return nil }

func TestChatRequestBodyOmitsStream(t *testing.T) {
	req := newLoadtestRequest("http://example.test", "gk-test", "chat")
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	_, ok := body["stream"]
	require.False(t, ok)
	require.Equal(t, "Bearer gk-test", req.Header.Get("Authorization"))
}

func TestStreamRequestBodyEnablesStream(t *testing.T) {
	req := newLoadtestRequest("http://example.test", "gk-test", "stream")
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	require.Equal(t, true, body["stream"])
}

// ---- fill 模式（管理面填充压测）----

// fillFlag 测试辅助：临时改全局 flag 值，defer 恢复（flag 包全局单例，
// 其余测试依赖默认 mode=stream）。
func fillFlag(t *testing.T, m, ft, tok string) {
	t.Helper()
	prevMode, prevType, prevTok := *mode, *fillType, *adminToken
	*mode, *fillType, *adminToken = m, ft, tok
	t.Cleanup(func() { *mode, *fillType, *adminToken = prevMode, prevType, prevTok })
}

func TestFillTypeMixedCycles(t *testing.T) {
	want := []string{"users", "keys", "accounts", "groups", "templates", "pricing"}
	for i, w := range want {
		require.Equal(t, w, fillTypeFor(int64(i+1)))
	}
	require.Equal(t, "users", fillTypeFor(7)) // 7 = 回到首轮
}

func TestFillRequestUsersBody(t *testing.T) {
	fillFlag(t, "fill", "users", "tok-test")
	fillProc = 42
	fillSeq.Store(0)
	req, preErr := newFillRequest(&http.Client{Timeout: time.Second}, rand.New(rand.NewPCG(1, 1)))
	require.Empty(t, preErr)
	require.Equal(t, "Bearer tok-test", req.Header.Get("Authorization"))
	require.Equal(t, "http://127.0.0.1:8080/admin/users", req.URL.String())
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	require.Equal(t, "fill-42-1@loadtest.test", body["email"])
	require.Equal(t, float64(100), body["balance"])
	require.Equal(t, float64(8), body["max_concurrency"])
}

func TestFillRequestNamesUniquePerRequest(t *testing.T) {
	fillFlag(t, "fill", "users", "tok")
	fillProc = 42
	fillSeq.Store(0)
	rng := rand.New(rand.NewPCG(1, 1))
	// 序号推进：同一进程内实体名必不重复（重复创建 409 归类错误明细的前提）
	names := map[string]bool{}
	for i := 0; i < 100; i++ {
		req, preErr := newFillRequest(&http.Client{Timeout: time.Second}, rng)
		require.Empty(t, preErr)
		var body map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		names[body["email"].(string)] = true
	}
	require.Len(t, names, 100)
}

func TestPickFillUser(t *testing.T) {
	prevUser, prevPool := *fillUser, fillUserPool
	*fillUser = "a@b.com:pw1"
	fillUserPool = []string{"c@d.com:pw2", "e@f.com:pw3"}
	t.Cleanup(func() { *fillUser, fillUserPool = prevUser, prevPool })
	rng := rand.New(rand.NewPCG(1, 1))
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		email, pw := pickFillUser(rng)
		seen[email+":"+pw] = true
	}
	require.Len(t, seen, 2)
	require.Contains(t, seen, "c@d.com:pw2")
	require.Contains(t, seen, "e@f.com:pw3")
	// 空池 → -fill-user 兜底
	fillUserPool = nil
	email, pw := pickFillUser(rng)
	require.Equal(t, "a@b.com", email)
	require.Equal(t, "pw1", pw)
}

func TestFillRequestSuccessRecordsLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, `{"ID":1}`)
	}))
	defer srv.Close()
	prevAddr := *addr
	*addr = srv.URL
	t.Cleanup(func() { *addr = prevAddr })
	fillFlag(t, "fill", "users", "tok")
	m := &metrics{errDetail: make(map[string]int64)}
	doFillRequest(&http.Client{Timeout: 5 * time.Second}, m, rand.New(rand.NewPCG(1, 1)), true)
	require.Equal(t, int64(1), m.total.Load())
	require.Equal(t, int64(0), m.errs.Load())
	require.GreaterOrEqual(t, p99Latency(m), int64(0)) // 成功 → latency 直方图有采样
}

func TestFillRequestNon200CountsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprintln(w, `{"error":"dup"}`)
	}))
	defer srv.Close()
	prevAddr := *addr
	*addr = srv.URL
	t.Cleanup(func() { *addr = prevAddr })
	fillFlag(t, "fill", "users", "tok")
	m := &metrics{errDetail: make(map[string]int64)}
	doFillRequest(&http.Client{Timeout: 5 * time.Second}, m, rand.New(rand.NewPCG(1, 1)), true)
	require.Equal(t, int64(1), m.total.Load())
	require.Equal(t, int64(1), m.errs.Load())
	m.mu.Lock()
	require.Equal(t, int64(1), m.errDetail["status:409"])
	m.mu.Unlock()
}
