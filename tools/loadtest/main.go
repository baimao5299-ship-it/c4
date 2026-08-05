// loadtest 对网关打流式压测：固定并发 goroutine 持续请求，统计首字节延迟/完成率/错误。
// 用法: go run ./tools/loadtest -addr http://127.0.0.1:8080 -key gk-xxx -concurrency 10000 -duration 5m -healthz http://127.0.0.1:8080/healthz
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
	key         = flag.String("key", "gk-", "group key")
	healthz     = flag.String("healthz", "", "gateway /healthz url to sample memory")
	out         = flag.String("out", "", "write RESULT summary to this file as well")
	pprof       = flag.String("pprof", "", "listen addr for /debug/pprof (goroutine dump on hang)")
)

type metrics struct {
	total       atomic.Int64
	errs        atomic.Int64
	firstByteMS atomic.Int64 // sum
	mu          sync.Mutex
	samples     map[int64]int64 // 首字节延迟采样：桶(ms/10) → 计数（估算 P99 用）
	errDetail   map[string]int64
}

func (m *metrics) addErr(detail string) {
	m.mu.Lock()
	m.errDetail[detail]++
	m.mu.Unlock()
}

func main() {
	flag.Parse()
	if *pprof != "" {
		go func() { _ = http.ListenAndServe(*pprof, nil) }() // net/http/pprof 自动挂载
	}
	m := &metrics{samples: make(map[int64]int64), errDetail: make(map[string]int64)}
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
	result := fmt.Sprintf("\n=== RESULT ===\ntotal=%d errs=%d avg_first_byte_ms=%.1f\n",
		m.total.Load(), m.errs.Load(), float64(m.firstByteMS.Load())/float64(max(1, m.total.Load())))
	result += fmt.Sprintf("p99_first_byte_ms=%d\n", p99(m))
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

// doRequest 执行一次流式请求；count=true 时计入 total/errs/首字节采样。
// 返回前对连接级失败做 100-300ms 抖动退避（见调用处注释）。
func doRequest(client *http.Client, m *metrics, count bool) {
	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, *addr+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+*key)
	req.Header.Set("Content-Type", "application/json")
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

func p99(m *metrics) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for _, c := range m.samples {
		total += c
	}
	if total == 0 {
		return -1
	}
	target := total * 99 / 100
	var acc int64
	for b := int64(0); ; b++ {
		acc += m.samples[b]
		if acc >= target {
			return b * 10
		}
		if b > 1_000_000 {
			return -1
		}
	}
}
