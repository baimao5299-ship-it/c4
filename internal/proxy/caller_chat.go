package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/openai/openai-go"
	"github.com/tidwall/gjson"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/pkg/sserelay"
)

// chatCaller 是 openai-chat 格式的 UpstreamCaller 实现（从 tryChat 迁移，
// 行为逐行等价）：流式走 SDK 原始请求 + sserelay，非流式走 SDK 参数路径；
// 记录职责全在本实现（finish/buildLog/recordStreamAbort/MarkResult）。
type chatCaller struct{ p *Proxy }

func (c *chatCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	tpl := tplOf(sel)

	if stream {
		// 客户端请求模型：流式无完整 params 解析（评审 I-2），gjson 顶层
		// 提取（1 次分配，远低于旧的完整参数解析）。ChatModel 即 string 别名。
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		// SDK NewStreaming 会在请求层注入 "stream": true；原始请求必须显式注入，
		// 否则上游按非流式响应，relay 收不到 SSE。注入后仍需发送原始 body。
		streamBody, err := setStreamFlag(body, true)
		if err != nil {
			return 0, nil, false, nil
		}
		// 模型改写：调度器选号已应用 ModelMapping（sel.Model 为上游模型名），
		// 与 SDK 路径 params.Model = sel.Model 等价，映射配置在流式下不失效。
		streamBody, err = setModel(streamBody, sel.Model)
		if err != nil {
			return 0, nil, false, nil
		}
		resp, err := p.clients.ChatCompletionStreamRaw(ctx, tpl, cred, streamBody)
		if err != nil {
			return statusOf(err), upstreamBody(err), false, nil
		}
		if resp.StatusCode != http.StatusOK {
			rb := readUpstreamBody(resp)
			resp.Body.Close()
			return resp.StatusCode, rb, false, nil
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
				// 对象/数组（缺失与显式 null 的 Type 均为 Null）。
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
				return 0, nil, true, nil
			}
			p.recordStreamAbort(reqID, start, sel, reqModel, err)
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, statusOf(err), err.Error())
			return 0, nil, true, nil
		}
		p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
		p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIChat, 200, domain.ErrNone, &usageTuple{pt: pt, ct: ct, tt: tt, cr: cr, cc: cc}, start))
		return 200, nil, true, nil
	}

	var params openai.ChatCompletionNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		// 本地拒绝（handled=true，无记录）：非流式 params 解析失败现状即
		// 本地 400、不记日志（评审 I-1 附加缺口）。Select 已占并发槽，必须
		// 释放（Release-only；finish(nil) 等价，直接 Release 更显式）。
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		p.sched.Release(sel.AccountID)
		return 400, nil, true, nil
	}
	// 客户端请求模型快照：下一行覆盖前取值（零额外分配，与 gjson 值等价）。
	reqModel := params.Model
	params.Model = sel.Model
	resp, err := p.clients.ChatCompletion(ctx, tpl, cred, params)
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
		// 非流式：cr 直读 SDK 结构体、cc 走 RawJSON() 原始字节 gjson 聚合
		// （评审 I-1 方案——结构体 marshal 自证不可用）。
		pt, ct, tt, cr, cc = chatUsageFromResponse(resp.Usage)
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
	p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIChat, 200, domain.ErrNone, &usageTuple{pt: pt, ct: ct, tt: tt, cr: cr, cc: cc}, start))
	return 200, nil, true, nil
}
