package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go"
	"github.com/tidwall/gjson"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/pkg/sserelay"
)

func (p *Proxy) HandleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := uuid.NewString()
	groupID, ok := p.auth.Authenticate(r)
	if !ok {
		writeErr(w, errInvalidKey)
		p.record(reqID, 0, 0, "", "", domain.FormatOpenAIChat, 401, domain.ErrAuth, 0, nil, start)
		return
	}
	if !p.limit.Allow(groupID, time.Now()) {
		writeErr(w, errRateLimit)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.cfg.MaxBodySize))
	if err != nil {
		writeErr(w, errBody)
		return
	}
	// openai-go v1.x 参数里没有 Stream 字段（流式由 NewStreaming 在请求选项层注入
	// "stream": true），故从原始请求体探测 stream 标志决定走流式还是非流式。
	var peek struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		return
	}
	var params openai.ChatCompletionNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		return
	}
	model := params.Model // ChatModel 即 string 别名

	sel, err := p.sched.Select(groupID, domain.FormatOpenAIChat, model)
	if err != nil {
		p.handleSelectError(w, err)
		p.record(reqID, groupID, 0, model, "", domain.FormatOpenAIChat, statusFor(err), domain.ErrNoAccount, 0, nil, start)
		return
	}

	var (
		lastCode int
		lastSel  = sel // 最后一次实际尝试的 Selection；中途 Select 失败返回 nil 时不得解引用 sel
	)
	for attempt := 0; attempt < p.cfg.FailoverAttempts; attempt++ {
		lastSel = sel
		ok, code, body := p.tryChat(w, r, reqID, groupID, start, sel, &params, peek.Stream, body)
		if ok {
			return // 已写出完整响应
		}
		lastCode = code
		if code == http.StatusTooManyRequests {
			p.sched.MarkResult(sel.AccountID, scheduler.Result429, nil, code, upstreamErrMsg(body))
		} else if code >= 500 || code == 0 {
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, code, upstreamErrMsg(body))
		} else {
			// 4xx 确定性错误：透传上游状态码与原始 body，不转移（规格 §5.3）；
			// body 不可得（连接级错误不会有 4xx 码）才回退网关文案。
			p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, model, sel.Model, domain.FormatOpenAIChat, code, domain.Err4xx, nil, start))
			if len(body) > 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = w.Write(body)
			} else {
				writeJSON(w, code, map[string]any{"error": map[string]any{
					"message": "upstream rejected request", "type": "upstream_error",
				}})
			}
			return
		}
		p.sched.Release(sel.AccountID)
		// 最后一轮不再为不存在的下一次尝试预选：尾部 Select 会抢占并发槽
		// （CAS 递增、仅 Release 递减、无回收），耗尽时永不释放 → 永久占槽。
		if attempt+1 >= p.cfg.FailoverAttempts {
			break
		}
		var selErr error
		sel, selErr = p.sched.Select(groupID, domain.FormatOpenAIChat, model)
		if selErr != nil {
			break
		}
	}
	// 耗尽：请求已完成（上游消费了请求），以最后一次尝试的结果记一条用量。
	et := domain.Err5xx
	switch {
	case lastCode == http.StatusTooManyRequests:
		et = domain.Err429
	case lastCode == 0:
		et = domain.ErrNetwork
	}
	p.record(reqID, groupID, lastSel.AccountID, model, lastSel.Model, domain.FormatOpenAIChat, lastCode, et, 0, nil, start)
	if lastCode == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	} else {
		writeErr(w, &formatError{status: http.StatusBadGateway, msg: "all upstream attempts failed"})
	}
}

// tryChat 返回 (已完整处理, 上游状态码, 上游错误 body)。流式 200 发出后无法转移。
// rbody 为 HandleChat 已读出的原始请求体（流式原始转发用）。
func (p *Proxy) tryChat(w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, params *openai.ChatCompletionNewParams, streaming bool, rbody []byte) (bool, int, []byte) {
	tpl := tplOf(sel)
	// 快照客户端请求模型：下一行 params.Model = sel.Model 覆盖后即丢失
	// （评审 I-1——model 变量只在 HandleXxx 作用域，try* 内无）。
	reqModel := params.Model
	params.Model = sel.Model

	if streaming {
		ctx, cancel := context.WithTimeout(r.Context(), p.cfg.UpstreamStreamTimeout)
		defer cancel()
		// SDK NewStreaming 会在请求层注入 "stream": true；原始请求必须显式注入，
		// 否则上游按非流式响应，relay 收不到 SSE。注入后仍需发送原始 body。
		streamBody, err := setStreamFlag(rbody, true)
		if err != nil {
			return false, 0, nil
		}
		// 模型改写：调度器选号已应用 ModelMapping（sel.Model 为上游模型名），
		// 与 SDK 路径 params.Model = sel.Model 等价，映射配置在流式下不失效。
		streamBody, err = setModel(streamBody, sel.Model)
		if err != nil {
			return false, 0, nil
		}
		resp, err := p.clients.ChatCompletionStreamRaw(ctx, tpl, sel.UpstreamKey, streamBody)
		if err != nil {
			return false, statusOf(err), upstreamBody(err)
		}
		if resp.StatusCode != http.StatusOK {
			body := readUpstreamBody(resp)
			resp.Body.Close()
			return false, resp.StatusCode, body
		}
		// SSE 响应头与旧 sseWriter 一致（relay 只转发字节，不代设头）
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		var pt, ct, tt, cr, cc int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Observer: func(ev sserelay.Event) {
				// "usage": null 的帧（存在但为 null）不得清零元组：Exists() 对
				// null 为真。gjson 无 IsNull()，用 Type == gjson.JSON 判定非空
				// 对象/数组（缺失与显式 null 的 Type 均为 Null）（评审 Minor）。
				if len(ev.Event) == 0 && gjson.GetBytes(ev.Data, "usage").Type == gjson.JSON {
					pt, ct, tt, cr, cc = chatStreamUsage(ev.Data)
				}
			},
		})
		resp.Body.Close()
		if err != nil {
			// 客户端断开：释放槽位，无法转移。不能按 errors.Is(err, context.Canceled)
			// 判断——sserelay.normalize 把任何 ctx 错误（含超时）折叠为 context.Canceled，
			// 超时只会取消子 ctx 而父 ctx（r.Context()）仍存活；上游停滞超时必须走
			// 上游错误分支（recordStreamAbort + ResultError），不得当作客户端断开。
			if r.Context().Err() != nil {
				// 客户端断开：上游已消费请求（成功），仍须记录用量，否则
				// 成功请求丢日志。与上游流中止同语义：200 + ErrAbort。
				p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIChat, http.StatusOK, domain.ErrAbort, &usageTuple{pt: pt, ct: ct, tt: tt, cr: cr, cc: cc}, start))
				return true, 0, nil
			}
			p.recordStreamAbort(reqID, start, sel, reqModel, err)
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, statusOf(err), err.Error())
			return true, 0, nil
		}
		p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
		p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIChat, 200, domain.ErrNone, &usageTuple{pt: pt, ct: ct, tt: tt, cr: cr, cc: cc}, start))
		return true, 200, nil
	}

	resp, err := p.clients.ChatCompletion(r.Context(), tpl, sel.UpstreamKey, *params)
	if err != nil {
		return false, statusOf(err), upstreamBody(err)
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return false, 0, nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	var pt, ct, tt, cr, cc int64
	if resp.JSON.Usage.Valid() {
		// 非流式：cr 直读 SDK 结构体、cc 走 RawJSON() 原始字节 gjson 聚合
		// （评审 I-1 方案——结构体 marshal 自证不可用）。
		pt, ct, tt, cr, cc = chatUsageFromResponse(resp.Usage)
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
	p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIChat, 200, domain.ErrNone, &usageTuple{pt: pt, ct: ct, tt: tt, cr: cr, cc: cc}, start))
	return true, 200, nil
}
