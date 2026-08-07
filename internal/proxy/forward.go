// Package proxy 是 AI 请求热路径：分组 key 鉴权 → 调度器选号 → SDK 转发 → 用量采集。
// 规格 §6/§9。不变量：热路径零 DB、零 per-request 锁。
package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"

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

// buildLog 组装 UsageLog（record 与 finish 共用）。语义（评审 I-1 确认）：
// Model = 客户端请求模型（reqModel），MappedModel = 映射后实际模型
// （usedModel 与请求模型不同才写入，否则空 = 未映射）。
func (p *Proxy) buildLog(reqID string, groupID, accountID int64, reqModel, usedModel string, format domain.RequestFormat, status int, et domain.ErrorType, u *usageTuple, start time.Time) *domain.UsageLog {
	if u == nil {
		u = &usageTuple{}
	}
	return &domain.UsageLog{
		RequestID: reqID, GroupID: groupID, AccountID: accountID,
		Model: reqModel, MappedModel: mappedFor(reqModel, usedModel), Format: format, StatusCode: status, ErrorType: et,
		LatencyMS:           time.Since(start).Milliseconds(),
		PromptTokens:        u.pt, CompletionTokens: u.ct, TotalTokens: u.tt,
		CacheReadTokens:     u.cr, CacheCreationTokens: u.cc,
		CreatedAt: time.Now(),
	}
}

// mappedFor 判定映射关系：实际使用的模型（used）非空且与请求模型（req）不同
// → 返回映射后模型；无映射/失败路径（used 为空或与请求相同）→ 空。精确比较
// 足够——ModelMapping 匹配语义即大小写敏感等值（selection.go）。
func mappedFor(req, used string) string {
	if used != "" && used != req {
		return used
	}
	return ""
}

// record 记录一条用量日志（无并发槽的失败路径；有槽路径走 finish）。
func (p *Proxy) record(reqID string, groupID, accountID int64, reqModel, usedModel string, format domain.RequestFormat, status int, et domain.ErrorType, latencyMS int64, u *usageTuple, start time.Time) {
	if !p.cfg.UsageCapture {
		return
	}
	p.rec.Record(p.buildLog(reqID, groupID, accountID, reqModel, usedModel, format, status, et, u, start))
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

// --- 辅助 ---

type usageTuple struct {
	pt, ct, tt     int64
	cr, cc         int64 // 缓存读取/写入 token（缺失 = 0）
}

func (p *Proxy) recordStreamAbort(reqID string, start time.Time, sel *scheduler.Selection, reqModel string, err error) {
	if p.log != nil {
		p.log.Warn("upstream stream aborted", logx.String("request_id", reqID), logx.Error(err))
	}
	p.finish(sel.AccountID, p.buildLog(reqID, 0, sel.AccountID, reqModel, sel.Model, sel.Format, 200, domain.ErrAbort, nil, start))
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

// upstreamBody 提取上游错误响应的原始 body：openai.Error / anthropic.Error 的
// RawJSON() 即收到的未修改 JSON 原文（apierror.Error.JSON.raw），4xx 透传用。
// 连接级/超时错误无 body，返回 nil。
func upstreamBody(err error) []byte {
	type rawJSONer interface{ RawJSON() string }
	var rj rawJSONer
	if errors.As(err, &rj) {
		if s := rj.RawJSON(); s != "" {
			return []byte(s)
		}
	}
	return nil
}

// upstreamErrMsg 提取上游错误 body 的 message（规则 when.error_message_contains
// 匹配用）：OpenAI/Anthropic 错误格式均为 {"error":{"message":...}}。
func upstreamErrMsg(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	s := gjson.GetBytes(body, "error.message").String()
	if s == "" {
		s = gjson.GetBytes(body, "message").String()
	}
	return s
}

// readUpstreamBody 读取并关闭非 200 响应的 body（4xx/5xx 透传用）。
func readUpstreamBody(resp *http.Response) []byte {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	return b
}

// setStreamFlag 把原始请求体里的 "stream" 字段设为给定值（流式注入用）。
// 用 map 重写避免对任意 JSON 结构做字符串手术；失败返回 nil。
func setStreamFlag(body []byte, v bool) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["stream"] = v
	return json.Marshal(m)
}

// setModel 把原始请求体里的 "model" 字段改写为调度器选定的上游模型名
// （ModelMapping 已应用，见 scheduler.Select 的 Selection.Model）。原始转发
// 必须沿用 SDK 路径 params.Model = sel.Model 的改写语义，否则映射配置在
// 流式请求上失效（Task 3 迁移发现）。
func setModel(body []byte, model string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	return json.Marshal(m)
}

// tplOf 从 Selection 构造轻量模板对象（仅用于 aiclient 取 SDK 客户端）。
// base_url 变更生效链路（main.go 组合注入）：管理端变更 → service 的 invalidate
// 回调 → 调度器 InvalidateAll 重载快照（新 base_url 随 Selection 下发）+ aiclient
// Factory.InvalidateAll 丢弃按旧 base_url 构建的 SDK 客户端（懒构建，下次使用重建）。
func tplOf(sel *scheduler.Selection) *domain.Template {
	return &domain.Template{ID: sel.TemplateID, BaseURL: sel.BaseURL}
}
