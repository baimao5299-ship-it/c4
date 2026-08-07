package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/openai/openai-go/responses"
	"github.com/tidwall/gjson"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/pkg/sserelay"
)

// responsesCaller 是 openai-responses 格式的 UpstreamCaller 实现（从 tryResponses
// 迁移，行为逐行等价）。
type responsesCaller struct{ p *Proxy }

func (c *responsesCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	tpl := tplOf(sel)
	// 客户端请求模型：gjson 顶层提取，零分配（ResponsesModel 即 string 别名）。
	reqModel := gjson.GetBytes(body, "model").String()

	if stream {
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		// 模型改写：与 SDK 路径 params.Model = sel.Model 等价（ModelMapping 语义）。
		// 客户端请求体已带 stream:true（fake 上游按 body["stream"] 分支），无需注入。
		streamBody, err := setModel(body, sel.Model)
		if err != nil {
			return 0, nil, false, nil
		}
		resp, err := p.clients.ResponseStreamRaw(ctx, tpl, cred, streamBody)
		if err != nil {
			return statusOf(err), upstreamBody(err), false, nil
		}
		if resp.StatusCode != http.StatusOK {
			rb := readUpstreamBody(resp)
			resp.Body.Close()
			return resp.StatusCode, rb, false, nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		var pt, ct, tt, cr, cc int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Observer: func(ev sserelay.Event) {
				// 用量只在 response.completed 事件携带（响应对象的 usage 字段；
				// 评审 M2：流式前缀 response.usage.*）。
				if bytes.Equal(ev.Event, []byte("response.completed")) {
					pt, ct, tt, cr, cc = responsesCompletedUsage(ev.Data)
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
				// 成功请求丢日志。与上游流中止同语义：200 + ErrAbort，
				// token 取断前已收到的 usage 帧（无则 0）。
				p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, http.StatusOK, domain.ErrAbort, &usageTuple{pt: pt, ct: ct, tt: tt, cr: cr, cc: cc}, start))
				return 0, nil, true, nil
			}
			p.recordStreamAbort(reqID, start, sel, reqModel, err)
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, statusOf(err), err.Error())
			return 0, nil, true, nil
		}
		p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
		p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, 200, domain.ErrNone, &usageTuple{pt: pt, ct: ct, tt: tt, cr: cr, cc: cc}, start))
		return 200, nil, true, nil
	}

	var params responses.ResponseNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		// 本地拒绝（handled=true，无记录）：同 chat 语义。Select 已占并发槽，
		// 必须释放（Release-only）。
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		p.sched.Release(sel.AccountID)
		return 400, nil, true, nil
	}
	params.Model = responses.ResponsesModel(sel.Model)
	resp, err := p.clients.Response(ctx, tpl, cred, params)
	if err != nil {
		return statusOf(err), upstreamBody(err), false, nil
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return 0, nil, false, nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	var pt, ct, tt, cr, cc int64
	if resp.JSON.Usage.Valid() {
		// 非流式：cr 直读 SDK 结构体、cc 走 RawJSON() gjson 聚合
		// （Responses 无 cache_creation 对象，恒 0 预期——M4）。
		pt, ct, tt, cr, cc = responsesUsageFromResponse(resp.Usage)
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
	p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, 200, domain.ErrNone, &usageTuple{pt: pt, ct: ct, tt: tt, cr: cr, cc: cc}, start))
	return 200, nil, true, nil
}
