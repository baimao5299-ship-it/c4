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

type metrics struct {
	total          atomic.Int64
	errs           atomic.Int64
	firstByteMS    atomic.Int64 // stream 首字节延迟之和
	latencyMS      atomic.Int64 // chat 完整响应延迟之和
	mu             sync.Mutex
	samples        map[int64]int64 // stream 首字节延迟采样：桶(ms/10) → 计数
	latencySamples map[int64]int64 // chat 完整响应延迟采样：桶(ms/10) → 计数
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
	m := &metrics{
		samples: make(map[int64]int64), latencySamples: make(map[int64]int64),
		errDetail: make(map[string]int64),
	}
	warmEnd := time.Now().Add(*warmup)
	start := warmEnd
	stop := start.Add(*duration)

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 10 * time.Minute}
			// 预热：先跑不计数的流，把突发拨号造成的 RST 吸收在计时窗口外，
			// 同时让 keep-alive 连接池就位（Windows backlog≈200，见错误退避注释）。
			for time.Now().Before(warmEnd) {
				doRequest(client, m, false)
			}
			for time.Now().Before(stop) {
				doRequest(client, m, true)
			}
		}()
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
func pickKey() string {
	if len(keyPool) > 0 {
		return keyPool[rand.IntN(len(keyPool))]
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

// doRequest executes one request; count=true includes it in the result metrics.
// Connection failures retain the jittered backoff used by the stream benchmark.
func doRequest(client *http.Client, m *metrics, count bool) {
	req := newLoadtestRequest(*addr, pickKey(), *mode)
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
		time.Sleep(time.Duration(100+rand.IntN(200)) * time.Millisecond)
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
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if strings.Contains(line, "[DONE]") || err != nil {
			break
		}
	}
	resp.Body.Close()
	if count {
		m.total.Add(1)
	}
}

func storeSample(m *metrics, v int64) {
	m.mu.Lock()
	m.samples[v/10]++
	m.mu.Unlock()
}

func storeLatencySample(m *metrics, v int64) {
	m.mu.Lock()
	m.latencySamples[v/10]++
	m.mu.Unlock()
}

func p99(m *metrics) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return p99Buckets(m.samples)
}

func p99Latency(m *metrics) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return p99Buckets(m.latencySamples)
}

func p99Buckets(samples map[int64]int64) int64 {
	var total int64
	for _, c := range samples {
		total += c
	}
	if total == 0 {
		return -1
	}
	target := total * 99 / 100
	var acc int64
	for b := int64(0); ; b++ {
		acc += samples[b]
		if acc >= target {
			return b * 10
		}
		if b > 1_000_000 {
			return -1
		}
	}
}
