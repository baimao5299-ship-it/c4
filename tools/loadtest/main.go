// loadtest 对网关打压测：固定并发 goroutine 持续请求，支持流式首字节和非流式完整响应延迟。
// 用法: go run ./tools/loadtest -mode stream -addr http://127.0.0.1:8080 -key gk-xxx -concurrency 10000 -duration 5m -healthz http://127.0.0.1:8080/healthz
//
// 相对 brief 原代码的修正（均标注在行内）：
//   - os import 用 -out 兜底：把 RESULT 摘要同时写入文件（验收记录留档）。
//   - 采样 goroutine 的 elapsed 直接取真实经过时间（brief 里 time.Since 套
//     time.Since 的表达式恒为 ~0s，属"简化输出"占位）。
//   - 首字节采样从 sync.Map 改为 mutex map（并发 CAS 实测在 Go 1.26
//     HashTrieMap 上高争用会活锁，压测卡死，见压测记录）。
//   - 连接级失败 100-300ms 抖动退避 + -warmup 预热窗口：Windows 监听
//     backlog≈200，无退避的突发拨号会形成自持 RST 拒绝风暴（500 并发
//     无退避实测 99.4% refused）。
//   - 错误明细（err_detail）输出 + -pprof goroutine 转储，便于定位压测问题。
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	concurrency = flag.Int("concurrency", 10000, "concurrent streams")
	duration    = flag.Duration("duration", 5*time.Minute, "test duration")
	warmup      = flag.Duration("warmup", 10*time.Second, "untimed connection warm-up before the clock starts")
	addr        = flag.String("addr", "http://127.0.0.1:8080", "gateway addr")
	key         = flag.String("key", "gk-", "single gateway key (fallback when -keys is empty)")
	keysFile    = flag.String("keys", "", "file with one gateway key per line; pick random per request (multi-key load)")
	format      = flag.String("format", "chat", "request format: chat, responses or anthropic")
	healthz     = flag.String("healthz", "", "gateway /healthz url to sample memory")
	out         = flag.String("out", "", "write RESULT summary to this file as well")
	pprof       = flag.String("pprof", "", "listen addr for /debug/pprof (goroutine dump on hang)")
	mode        = flag.String("mode", "stream", "request mode: stream or chat")
)

// keyPool 多 key 模式：每请求随机取一个（-keys 文件行）；空 = 用 -key 单 key。
var keyPool []string

// 采样桶数：10ms/桶 → 覆盖 0-10.24s；超出区间（≥10.24s）进边界桶 1023。
const sampleBuckets = 1024

type metrics struct {
	total       atomic.Int64
	errs        atomic.Int64
	firstByteMS atomic.Int64 // stream 首字节延迟之和
	latencyMS   atomic.Int64 // chat 完整响应延迟之和
	// 延迟采样直方图：固定桶数组 + 原子自增，无锁。原 mutex map 在
	// 30k 并发下每请求抢同一把锁（最大热点）；p99 遍历数组同样无锁。
	samples        [sampleBuckets]atomic.Int64 // stream 首字节延迟采样：10ms/桶
	latencySamples [sampleBuckets]atomic.Int64 // chat 完整响应延迟采样：10ms/桶
	mu             sync.Mutex                  // 仅保护 errDetail（错误路径低频，0 错误零锁）
	errDetail      map[string]int64
}

func (m *metrics) addErr(detail string) {
	m.mu.Lock()
	m.errDetail[detail]++
	m.mu.Unlock()
}

func main() {
	flag.Parse()
	if *mode != "stream" && *mode != "chat" {
		fmt.Fprintf(os.Stderr, "invalid -mode %q: want stream or chat\n", *mode)
		os.Exit(2)
	}
	if *format != "chat" && *format != "responses" && *format != "anthropic" {
		fmt.Fprintf(os.Stderr, "invalid -format %q: want chat, responses or anthropic\n", *format)
		os.Exit(2)
	}
	if *keysFile != "" {
		b, err := os.ReadFile(*keysFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read -keys %s: %v\n", *keysFile, err)
			os.Exit(2)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				keyPool = append(keyPool, line)
			}
		}
		if len(keyPool) == 0 {
			fmt.Fprintf(os.Stderr, "-keys %s: no keys found\n", *keysFile)
			os.Exit(2)
		}
		fmt.Printf("loaded %d keys from %s\n", len(keyPool), *keysFile)
	}
	if *pprof != "" {
		go func() { _ = http.ListenAndServe(*pprof, nil) }() // net/http/pprof 自动挂载
	}
	m := &metrics{errDetail: make(map[string]int64)}
	buildReqTemplate()
	warmEnd := time.Now().Add(*warmup)
	start := warmEnd
	stop := start.Add(*duration)

	// transport 全局共享（http.Client 每次请求新建、transport 复用）：DefaultTransport
	// 的 MaxIdleConns/MaxIdleConnsPerHost 都是 100，50k 并发下连接池形同虚设——
	// 流一结束连接就被关、下一请求重新拨号（每请求一次拨号，三进程 13k/s 的
	// 连接风暴，压测工具在测自己的拨号开销而非网关）。给足池容量（= 并发数，
	// 稳态下每 goroutine 恰好持一条 keep-alive 连接反复复用），拨号率趋近 0。
	transport := &http.Transport{
		MaxIdleConns:        *concurrency,
		MaxIdleConnsPerHost: *concurrency,
		IdleConnTimeout:     90 * time.Second,
	}
	// 每 worker 自建局部随机源：math/rand/v2 全局源带锁，30k 并发下
	// pickKey/退避抢同一把锁；按 worker 索引分种子，退避不会同频共振。
	randSeed := uint64(time.Now().UnixNano())
	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(rng *rand.Rand) {
			defer wg.Done()
			client := &http.Client{Timeout: 10 * time.Minute, Transport: transport}
			// 预热：先跑不计数的流，把突发拨号造成的 RST 吸收在计时窗口外，
			// 同时让 keep-alive 连接池就位（Windows backlog≈200，见错误退避注释）。
			for time.Now().Before(warmEnd) {
				doRequest(client, m, rng, false)
			}
			for time.Now().Before(stop) {
				doRequest(client, m, rng, true)
			}
		}(rand.New(rand.NewPCG(randSeed, uint64(i+1))))
	}

	// 采样 goroutine：打印即时进度 + /healthz 内存
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		var last int64
		for range t.C {
			cur := m.total.Load()
			fmt.Printf("elapsed=%s total=%d rate=%.0f/s errs=%d\n",
				time.Since(start).Round(time.Second),
				cur, float64(cur-last)/10, m.errs.Load())
			last = cur
			if *healthz != "" {
				if resp, err := http.Get(*healthz); err == nil {
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					fmt.Printf("  gateway: %s\n", b)
				}
			}
		}
	}()

	wg.Wait()
	result := fmt.Sprintf("\n=== RESULT ===\nmode=%s\ntotal=%d errs=%d\n", *mode, m.total.Load(), m.errs.Load())
	if *mode == "chat" {
		result += fmt.Sprintf("avg_latency_ms=%.1f\n", float64(m.latencyMS.Load())/float64(max(1, m.total.Load()-m.errs.Load())))
		result += fmt.Sprintf("p99_latency_ms=%d\n", p99Latency(m))
	} else {
		result += fmt.Sprintf("avg_first_byte_ms=%.1f\n", float64(m.firstByteMS.Load())/float64(max(1, m.total.Load()-m.errs.Load())))
		result += fmt.Sprintf("p99_first_byte_ms=%d\n", p99(m))
	}
	result += fmt.Sprintf("elapsed=%s concurrency=%d\n", time.Since(start).Round(time.Second), *concurrency)
	m.mu.Lock()
	for d, c := range m.errDetail {
		if c >= 1 {
			result += fmt.Sprintf("err_detail: %d x %s\n", c, d)
		}
	}
	m.mu.Unlock()
	fmt.Print(result)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(result), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		}
	}
}

// pickKey 每请求选 key：多 key 池随机（-keys 文件）→ 单 key（-key）兜底。
// rng 为 worker 局部随机源（全局 rand 带锁，见 main 内注释）。
func pickKey(rng *rand.Rand) string {
	if len(keyPool) > 0 {
		return keyPool[rng.IntN(len(keyPool))]
	}
	return *key
}

// newLoadtestRequest builds the minimal request for the selected mode + format.
// 三格式（多模板多格式压测）：chat → /v1/chat/completions（Bearer），
// responses → /v1/responses（Bearer），anthropic → /v1/messages（x-api-key）。
func newLoadtestRequest(base, groupKey, requestMode string) *http.Request {
	var path, body string
	switch *format {
	case "responses":
		path = "/v1/responses"
		body = `{"model":"gpt-4o","input":"hi"}`
		if requestMode == "stream" {
			body = `{"model":"gpt-4o","stream":true,"input":"hi"}`
		}
	case "anthropic":
		path = "/v1/messages"
		body = `{"model":"claude-3-5-sonnet-20241022","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
		if requestMode == "stream" {
			body = `{"model":"claude-3-5-sonnet-20241022","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
	default: // chat
		path = "/v1/chat/completions"
		body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
		if requestMode == "stream" {
			body = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
	}
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader([]byte(body)))
	if *format == "anthropic" {
		req.Header.Set("x-api-key", groupKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+groupKey)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// 请求模板：format×mode 在进程内固定，URL/body/固定头只构建一次；每请求
// Clone + 换 key，避免 http.NewRequest 的 URL 解析 + body 字符串复制 +
// 头表构建（4 万+ req/s 下是 GC 的主要来源之一，见上机 profile）。
var (
	reqTmpl  *http.Request
	tmplBody []byte
)

// buildReqTemplate 按当前 flags 预构建请求模板（main 启动时调用一次）。
func buildReqTemplate() {
	reqTmpl = newLoadtestRequest(*addr, *key, *mode)
	tmplBody, _ = io.ReadAll(reqTmpl.Body)
	reqTmpl.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(tmplBody)), nil
	}
}

// newRequestFromTemplate 克隆模板并按 key 设置认证头（其余静态）。
func newRequestFromTemplate(groupKey string) *http.Request {
	req := reqTmpl.Clone(context.Background())
	if *format == "anthropic" {
		req.Header.Set("x-api-key", groupKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+groupKey)
	}
	req.Body = io.NopCloser(bytes.NewReader(tmplBody))
	req.ContentLength = int64(len(tmplBody))
	return req
}

// doRequest executes one request; count=true includes it in the result metrics.
// Connection failures retain the jittered backoff used by the stream benchmark.
func doRequest(client *http.Client, m *metrics, rng *rand.Rand, count bool) {
	req := newRequestFromTemplate(pickKey(rng))
	reqStart := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if count {
			m.errs.Add(1)
			m.total.Add(1)
			m.addErr("do:" + err.Error())
		}
		// 连接级失败退避（100-300ms 抖动）：Windows 监听 backlog（SOMAXCONN≈200）
		// 下突发拨号会被 RST，无退避的立即重试会形成自持拒绝风暴
		// （Task 9 实测：500 并发无退避 99.4% refused，150 并发无失败）。
		time.Sleep(time.Duration(100+rng.IntN(200)) * time.Millisecond)
		return
	}
	if resp.StatusCode != 200 {
		if count {
			m.errs.Add(1)
			m.total.Add(1)
			m.addErr(fmt.Sprintf("status:%d", resp.StatusCode))
		}
		resp.Body.Close()
		return
	}
	if *mode == "chat" {
		_, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			if count {
				m.errs.Add(1)
				m.total.Add(1)
				m.addErr("read:" + readErr.Error())
			}
			return
		}
		if count {
			latency := time.Since(reqStart).Milliseconds()
			m.latencyMS.Add(latency)
			storeLatencySample(m, latency)
			m.total.Add(1)
		}
		return
	}
	if count {
		firstByte := time.Since(reqStart).Milliseconds()
		m.firstByteMS.Add(firstByte)
		storeSample(m, firstByte)
	}
	br := sseReaderPool.Get().(*bufio.Reader)
	br.Reset(resp.Body)
	_ = drainSSE(br) // 排空尽力而为：读错误（服务端提前断流）不影响压测统计
	// [DONE] 后把响应体尾部读到 EOF（chunked/Content-Length 的 EOF 由帧结构
	// 决定，µs 级到达）：让传输层判定 body 完整、keep-alive 连接回池复用——
	// 原实现每请求关连接重拨（profile 里 dialConn 占 11% CPU）。服务端不
	// 结束流时 drainTail 50ms 超时放弃，不挂死。
	_ = drainTail(resp.Body) // 超时/EOF 均属预期，尽力而为
	br.Reset(nil)
	sseReaderPool.Put(br)
	resp.Body.Close()
	if count {
		m.total.Add(1)
	}
}

// sseReaderPool 复用每请求的 bufio.Reader（4KB 缓冲，4 万+ req/s 下零化/
// 分配量可观）；回池前 Reset(nil) 丢弃跨连接残留缓冲数据。
var sseReaderPool = sync.Pool{New: func() any { return bufio.NewReader(nil) }}

// errDrainTimeout [DONE] 后服务端未在 50ms 内结束响应体：放弃该连接复用。
var errDrainTimeout = errors.New("drainTail: body not ended within 50ms")

// drainTail [DONE] 后把响应体剩余字节读到 EOF，使传输层判定 body 完整 →
// 连接回池复用；服务端不主动结束流时 50ms 超时 Close 放弃（Close 会解除
// 阻塞中的 Read，不留泄漏 goroutine）。返回 nil 表示读到 EOF（连接可复用）。
func drainTail(body io.ReadCloser) error {
	drained := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, body); drained <- err }()
	select {
	case err := <-drained:
		return err
	case <-time.After(50 * time.Millisecond):
		body.Close()
		return errDrainTimeout
	}
}

// sseDone 每行检查复用同一 []byte，避免逐行转换分配。
var sseDone = []byte("[DONE]")

// drainSSE 读完整条流直到 [DONE] 行（返回 nil）或流结束（返回读取错误）。
// 原 ReadString 每行分配一个 string，30k 并发下堆压力大；ReadSlice 复用
// bufio 内部缓冲零分配，长行（ErrBufferFull）分段累积到局部 buf，行尾
// 一次性检查。注意 ReadSlice 返回的 slice 在下次读取后即失效，须立即消费。
func drainSSE(br *bufio.Reader) error {
	var buf []byte
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull { // 长行：累积分段，等后续续行
			buf = append(buf, line...)
			continue
		}
		if len(buf) > 0 {
			line = append(buf, line...) // 行完整：拼接分段后立即检查
			buf = buf[:0]
		}
		if bytes.Contains(line, sseDone) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func storeSample(m *metrics, v int64) {
	idx := v / 10
	if idx >= sampleBuckets {
		idx = sampleBuckets - 1
	}
	m.samples[idx].Add(1)
}

func storeLatencySample(m *metrics, v int64) {
	idx := v / 10
	if idx >= sampleBuckets {
		idx = sampleBuckets - 1
	}
	m.latencySamples[idx].Add(1)
}

func p99(m *metrics) int64 { return p99Buckets(&m.samples) }

func p99Latency(m *metrics) int64 { return p99Buckets(&m.latencySamples) }

// p99Buckets 遍历固定桶数组求 99% 分位（无锁），语义与旧 mutex map 版
// 一致：从低到高累积计数，首个累积 ≥ total*99/100 的桶返回上界（b*10 ms）。
func p99Buckets(samples *[sampleBuckets]atomic.Int64) int64 {
	var total int64
	for i := range samples {
		total += samples[i].Load()
	}
	if total == 0 {
		return -1
	}
	target := total * 99 / 100
	var acc int64
	for i := range samples {
		acc += samples[i].Load()
		if acc >= target {
			return int64(i) * 10
		}
	}
	return -1 // 不可达：桶总和 = total > target，必有桶越过阈值
}
