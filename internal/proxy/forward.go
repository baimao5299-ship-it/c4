// Package proxy 是 AI 请求热路径：分组 key 鉴权 → 调度器选号 → SDK 转发 → 用量采集。
// 规格 §6/§9。不变量：热路径零 DB、零 per-request 锁。
package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/internal/usage"
	"go-proxy-mini/pkg/aiclient"
	"go-proxy-mini/pkg/logx"
)

type Config struct {
	MaxBodySize           int64
	MaxInflight           int64
	UpstreamStreamTimeout time.Duration // 流式 backstop（非流式超时在 aiclient.Config）
	FailoverAttempts      int
	GroupKeyRPM           int
	UsageCapture          bool
}

type Proxy struct {
	cfg      Config
	sched    *scheduler.Scheduler
	rec      *usage.Recorder
	clients  *aiclient.Factory
	auth     *Auth
	limit    *fixedWindowLimiter
	log      *logx.Logger
	inflight atomic.Int64
}

func New(cfg Config, sched *scheduler.Scheduler, rec *usage.Recorder, clients *aiclient.Factory, auth *Auth, log *logx.Logger) *Proxy {
	return &Proxy{
		cfg: cfg, sched: sched, rec: rec, clients: clients, auth: auth,
		limit: newFixedWindowLimiter(cfg.GroupKeyRPM), log: log,
	}
}

func (p *Proxy) Inflight() int64 { return p.inflight.Load() }

// finish 收尾：释放并发槽 + 记录用量（凡持有并发槽的路径必调）。
func (p *Proxy) finish(accountID int64, l *domain.UsageLog) {
	p.sched.Release(accountID)
	if p.cfg.UsageCapture && l != nil {
		p.rec.Record(l)
	}
}

// buildLog 组装 UsageLog（record 与 finish 共用）。
func (p *Proxy) buildLog(reqID string, groupID, accountID int64, model string, format domain.RequestFormat, status int, et domain.ErrorType, u *usageTuple, start time.Time) *domain.UsageLog {
	if u == nil {
		u = &usageTuple{}
	}
	return &domain.UsageLog{
		RequestID: reqID, GroupID: groupID, AccountID: accountID,
		Model: model, Format: format, StatusCode: status, ErrorType: et,
		LatencyMS:    time.Since(start).Milliseconds(),
		PromptTokens: u.pt, CompletionTokens: u.ct, TotalTokens: u.tt,
		CreatedAt: time.Now(),
	}
}

// record 记录一条用量日志（无并发槽的失败路径；有槽路径走 finish）。
func (p *Proxy) record(reqID string, groupID, accountID int64, model string, format domain.RequestFormat, status int, et domain.ErrorType, latencyMS int64, u *usageTuple, start time.Time) {
	if !p.cfg.UsageCapture {
		return
	}
	p.rec.Record(p.buildLog(reqID, groupID, accountID, model, format, status, et, u, start))
}

type formatError struct {
	status int
	msg    string
}

func (e *formatError) Error() string { return e.msg }

var (
	errInvalidKey = &formatError{status: http.StatusUnauthorized, msg: "invalid gateway key"}
	errTooMany    = &formatError{status: http.StatusTooManyRequests, msg: "no available account"}
	errRateLimit  = &formatError{status: http.StatusTooManyRequests, msg: "group rate limited"}
	errBody       = &formatError{status: http.StatusRequestEntityTooLarge, msg: "request body too large"}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *formatError) {
	writeJSON(w, e.status, map[string]any{"error": map[string]any{"message": e.msg, "type": "gateway_error"}})
}

// --- SSE 写出（bufio 复用） ---

var bufioPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, 8192) }}

// sseWriter 复用 bufio 写出 SSE。fl 在每次事件后 Flush：
// 只刷 bufio 不刷 http.Flusher 时，Go http.Server 内部 4KB 缓冲会攒批放出，
// 首字节延迟被拉高到 ~145ms（Task 9 压测实测，修复后 ~1ms，
// 详见 docs/superpowers/plans/loadtest-results.md）。
type sseWriter struct {
	bw *bufio.Writer
	fl http.Flusher
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	bw := bufioPool.Get().(*bufio.Writer)
	bw.Reset(w)
	fl, _ := w.(http.Flusher) // 中间件包装（statusWriter）不实现 Flusher 时静默跳过
	return &sseWriter{bw: bw, fl: fl}
}

// Event 写出纯 data: 事件（openai SSE 风格）。
func (s *sseWriter) Event(v any) error {
	return s.write("", v)
}

// EventTyped 写出 event: <type> + data: 事件（anthropic SSE 风格：官方 SDK 按
// event: 行类型分发，纯 data 事件被静默跳过 → 流为空）。
func (s *sseWriter) EventTyped(eventType string, v any) error {
	return s.write(eventType, v)
}

func (s *sseWriter) write(eventType string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if eventType != "" {
		if _, err := s.bw.WriteString("event: " + eventType + "\n"); err != nil {
			return err
		}
	}
	if _, err := s.bw.WriteString("data: "); err != nil {
		return err
	}
	if _, err := s.bw.Write(data); err != nil {
		return err
	}
	if _, err := s.bw.WriteString("\n\n"); err != nil {
		return err
	}
	if err := s.bw.Flush(); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush() // 事件级冲刷（SSE 语义必需，见 newSSEWriter 注释）
	}
	return nil
}

func (s *sseWriter) Done() error {
	if _, err := s.bw.WriteString("data: [DONE]\n\n"); err != nil {
		return err
	}
	if err := s.bw.Flush(); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	bufioPool.Put(s.bw)
	return nil
}

func (s *sseWriter) Abort() { bufioPool.Put(s.bw) }

// --- 辅助 ---

type usageTuple struct {
	pt, ct, tt int64
}

func (p *Proxy) recordStreamAbort(reqID string, start time.Time, sel *scheduler.Selection, err error) {
	if p.log != nil {
		p.log.Warn("upstream stream aborted", logx.String("request_id", reqID), logx.Error(err))
	}
	p.finish(sel.AccountID, p.buildLog(reqID, 0, sel.AccountID, sel.Model, sel.Format, 200, domain.ErrAbort, nil, start))
}

func (p *Proxy) handleSelectError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, scheduler.ErrFormatUnavailable):
		writeErr(w, &formatError{status: http.StatusNotFound, msg: "no account supports this request format"})
	case errors.Is(err, scheduler.ErrNoAvailable):
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	default:
		writeErr(w, &formatError{status: http.StatusNotFound, msg: "group not found"})
	}
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, scheduler.ErrFormatUnavailable), errors.Is(err, scheduler.ErrGroupNotFound):
		return http.StatusNotFound
	default:
		return http.StatusTooManyRequests
	}
}

// statusOf 提取上游错误的状态码：优先 StatusCode() 方法；openai-go/anthropic
// v1.x 的 apierror.Error 用 StatusCode 字段暴露，退回反射读取。反射仅在错误
// 路径（罕见）执行，不占热路径。
func statusOf(err error) int {
	type statusCoder interface{ StatusCode() int }
	var sc statusCoder
	if errors.As(err, &sc) {
		return sc.StatusCode()
	}
	v := reflect.ValueOf(err)
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && !v.IsNil() {
		v = v.Elem()
	}
	if v.IsValid() && v.Kind() == reflect.Struct {
		if f := v.FieldByName("StatusCode"); f.IsValid() && f.Kind() == reflect.Int {
			return int(f.Int())
		}
	}
	return 0 // 连接级/超时错误
}

// tplOf 从 Selection 构造轻量模板对象（仅用于 aiclient 取 SDK 客户端；模板变更经 InvalidateAll 生效）。
func tplOf(sel *scheduler.Selection) *domain.Template {
	return &domain.Template{ID: sel.TemplateID, BaseURL: sel.BaseURL}
}
