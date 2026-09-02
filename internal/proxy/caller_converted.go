// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/protoconv"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// convertedCaller 是协议转换路径的 UpstreamCaller（W5）：请求体已由
// handleFormat 按方向转换（route.body，含 stream/model 字段），本实现按模板
// 协议调用上游（与 responsesCaller/anthropicCaller 同构），响应反向转换回
// 客户端协议：
//   - 流式：每帧经 protoconv.StreamMapper 映射后写出（sserelay Mapper；
//     Observer 仍见原始帧 → 用量提取与模板 caller 逐字同构）
//   - 非流式：上游响应 JSON 整体 ConvertResponse 转换
//
// 日志按客户端协议记录（buildLog format 参数 = 客户端格式——客户端视角的
// 请求格式）；用量提取仍按模板协议（上游字节不变）。
type convertedCaller struct {
	p   *Proxy
	dir domain.ProtocolConvert
}

func (c *convertedCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	client, target := clientAndTargetOf(c.dir)
	if err := validateRequestParameterTypes(target, body); err != nil {
		return p.rejectLocalRequest(ctx, w, reqID, groupID, start, sel, client, body, err)
	}

	if stream {
		// 客户端请求模型：转换器保证 model 字段原样保留（补差映射），gjson
		// 顶层提取与模板 caller 同构。
		reqModel := gjson.GetBytes(body, "model").String()
		ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
		defer cancel()
		// 模型改写（ModelMapping 语义）：转换后请求体已含 stream:true（转换器
		// 映射客户端 stream 标志），setModel 短路守卫与模板 caller 同构。
		streamBody, err := setModel(body, sel.Model)
		if err != nil {
			return 0, nil, false, err
		}
		if target == domain.FormatOpenAIChat {
			streamBody, err = ensureChatStreamUsage(streamBody)
			if err != nil {
				return 0, nil, false, err
			}
		}
		var resp *http.Response
		switch target {
		case domain.FormatOpenAIChat:
			resp, err = p.clients.ChatCompletionStreamRaw(ctx, sel.TemplateID, sel.BaseURL, cred, streamBody)
		case domain.FormatOpenAIResponses:
			resp, err = p.clients.ResponseStreamRaw(ctx, sel.TemplateID, sel.BaseURL, cred, streamBody)
		case domain.FormatAnthropic:
			resp, err = p.clients.AnthMessageStreamRaw(ctx, sel.TemplateID, sel.BaseURL, cred, streamBody)
		}
		if err != nil {
			return statusOf(err), upstreamBody(err), false, err
		}
		if resp.StatusCode != http.StatusOK {
			rb := readUpstreamBody(resp)
			hdr := cloneResponseHeaders(resp.Header)
			resp.Body.Close()
			return resp.StatusCode, rb, false, &responseHeadersError{header: hdr}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		mapper := protoconv.NewStreamMapper(c.dir)
		var it, ot, tt, cr, cc int64
		var streamFailure error
		// TTFT 采集（首 token 时间毫秒）：与模板 caller 同构。
		var ttft *int64
		err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
			Mapper: func(ev sserelay.Event) ([]byte, bool) {
				if ttft == nil {
					ms := time.Since(start).Milliseconds()
					ttft = &ms
				}
				// 用量提取走原始帧（与模板 caller 逐字同构；映射只影响写出字节）。
				// EventName：缺 event: 名帧按 data.type 推断（非规范上游，P3）。
				switch target {
				case domain.FormatOpenAIChat:
					if bytes.Contains(ev.Data, []byte(`"usage"`)) {
						if t, ok := chatStreamUsageEvent(ev.EventName(), ev.Data); ok {
							name := ev.EventName()
							if bytes.Equal(name, []byte("message_start")) || bytes.Equal(name, []byte("message_delta")) {
								if t.it > 0 {
									it = t.it
								}
								if t.ot > 0 {
									ot = t.ot
								}
								if t.cr > 0 {
									cr = t.cr
								}
								if t.cc > 0 {
									cc = t.cc
								}
							} else {
								// Keep scalar locals for this hot path while applying the
								// same non-erasing merge semantics as caller_chat.
								if t.it > 0 {
									it = t.it
								}
								if t.ot > 0 {
									ot = t.ot
								}
								if t.tt > 0 {
									tt = t.tt
								}
								if t.cr > 0 {
									cr = t.cr
								}
								if t.cc > 0 {
									cc = t.cc
								}
							}
						}
					}
				case domain.FormatOpenAIResponses:
					if isResponsesCompletedEvent(ev.EventName(), ev.Data) {
						if t, ok := responsesStreamUsage(ev.Data); ok {
							it, ot, tt, cr, cc = t.it, t.ot, t.tt, t.cr, t.cc
						}
					}
				case domain.FormatAnthropic:
					if t, ok := chatStreamUsageEvent(ev.EventName(), ev.Data); ok {
						if t.it > 0 {
							it = t.it
						}
						if t.ot > ot {
							ot = t.ot
						}
						if t.cr > 0 {
							cr = t.cr
						}
						if t.cc > 0 {
							cc = t.cc
						}
					}
				}
				if streamFailure == nil {
					streamFailure = convertedStreamFailure(ev)
				}
				return mapper.Map(string(ev.Event), ev.Data)
			},
		})
		// A provider can send a terminal failure event and then close the stream
		// cleanly. Relay quite correctly returns nil in that case; promote the
		// application failure before the success bookkeeping below.
		if err == nil && streamFailure != nil {
			err = streamFailure
		}
		if err == nil && target == domain.FormatOpenAIChat {
			// Some compatible Chat relays close immediately after the final JSON
			// chunk without emitting the conventional [DONE] sentinel. Complete
			// the client protocol once at EOF; the mapper drops this synthetic
			// sentinel when the real one already completed the stream.
			if tail, drop := mapper.Map("", []byte("[DONE]")); !drop {
				if _, writeErr := w.Write(tail); writeErr != nil {
					err = writeErr
				} else if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
		resp.Body.Close()
		if ttft != nil {
			ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
		}
		if tt <= 0 {
			tt = addUsageTokens(it, ot)
		}
		if err != nil {
			if streamFailure != nil {
				msg := domain.TruncateErrMsg(streamFailure.Error())
				l := logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, http.StatusBadGateway, domain.Err5xx, convertedUsageTuple(target, it, ot, tt, cr, cc), start))
				l.ErrorMessage = &msg
				p.finishSelection(sel, l)
				p.sched.MarkSelectionResult(sel, rule.Kind5xx, nil, http.StatusBadGateway, msg, sel.Model)
				return http.StatusBadGateway, nil, true, nil
			}
			// 客户端断开/流中止语义与模板 caller 逐字同构（recordStreamAbort +
			// MarkResult；客户端断开 finish ErrAbort 不转移）。errors.Is(err,
			// context.Canceled) 即客户端断开——sserelay.normalize 已区分三类
			// （C-P2-2）：上游停滞超时 → DeadlineExceeded 走上游错误分支。
			if errors.Is(err, context.Canceled) {
				p.finishSelection(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, http.StatusOK, domain.ErrAbort, convertedUsageTuple(target, it, ot, tt, cr, cc), start)))
				return 0, nil, true, nil
			}
			p.recordStreamAbortForFormat(ctx, reqID, groupID, start, sel, reqModel, client, convertedUsageTuple(target, it, ot, tt, cr, cc), err)
			p.sched.MarkSelectionResult(sel, scheduler.RuleKindOf(statusOf(err)), nil, statusOf(err), err.Error(), sel.Model)
			return 0, nil, true, nil
		}
		p.sched.MarkSelectionResult(sel, rule.KindOK, nil, http.StatusOK, "", sel.Model)
		p.finishSelection(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, 200, domain.ErrNone, convertedUsageTuple(target, it, ot, tt, cr, cc), start)))
		return 200, nil, true, nil
	}

	// 非流式：以模板协议参数解析（转换后请求体）→ SDK 调用 → 响应 JSON 反向
	// 转换回客户端协议。reqModel 在参数覆盖前从转换体提取（model 原样保留）。
	reqModel := gjson.GetBytes(body, "model").String()
	var data []byte
	var it, ot, tt, cr, cc int64
	tpl := tplOf(sel) // 非流式 SDK 路径（流式原始请求路径免模板对象分配）
	var upstreamErr error
	switch target {
	case domain.FormatOpenAIChat:
		var params openai.ChatCompletionNewParams
		if err := json.Unmarshal(body, &params); err != nil {
			msg := "invalid request body: " + err.Error()
			p.recordRejected(ctx, reqID, groupID, sel.AccountID, reqModel,
				sel.Model, client, http.StatusBadRequest, domain.Err4xx, 0,
				usageTuple{}, start, localRejectionMessage("invalid request body", err), sel)
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": msg}})
			p.sched.ReleaseSelection(sel)
			return 400, nil, true, nil
		}
		params.Model = sel.Model
		var resp *openai.ChatCompletion
		resp, upstreamErr = p.clients.ChatCompletion(ctx, tpl, cred, params)
		if upstreamErr == nil {
			data, upstreamErr = json.Marshal(resp)
			if resp.JSON.Usage.Valid() {
				it, ot, tt, cr, cc = chatUsageFromResponse(resp.Usage)
			}
		}
	case domain.FormatOpenAIResponses:
		var params responses.ResponseNewParams
		if err := json.Unmarshal(body, &params); err != nil {
			// 本地参数拒绝：已选号但尚未调用 SDK/上游。记录 err_logs 并保留
			// 选择快照，便于定位是哪一个上游候选被参数错误占用过。
			msg := "invalid request body: " + err.Error()
			p.recordRejected(ctx, reqID, groupID, sel.AccountID, reqModel,
				sel.Model, client, http.StatusBadRequest, domain.Err4xx, 0,
				usageTuple{}, start, localRejectionMessage("invalid request body", err), sel)
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": msg}})
			p.sched.ReleaseSelection(sel)
			return 400, nil, true, nil
		}
		params.Model = responses.ResponsesModel(sel.Model)
		var resp *responses.Response
		resp, upstreamErr = p.clients.Response(ctx, tpl, cred, params)
		if upstreamErr == nil {
			data, upstreamErr = json.Marshal(resp)
			if resp.JSON.Usage.Valid() {
				it, ot, tt, cr, cc = responsesUsageFromResponse(resp.Usage)
			}
		}
	case domain.FormatAnthropic:
		var params anthropic.MessageNewParams
		if err := json.Unmarshal(body, &params); err != nil {
			// 同上：转换目标参数在本地失败，目标上游尚未收到请求。
			msg := "invalid request body: " + err.Error()
			p.recordRejected(ctx, reqID, groupID, sel.AccountID, reqModel,
				sel.Model, client, http.StatusBadRequest, domain.Err4xx, 0,
				usageTuple{}, start, localRejectionMessage("invalid request body", err), sel)
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": msg}})
			p.sched.ReleaseSelection(sel)
			return 400, nil, true, nil
		}
		params.Model = sel.Model // Model = string 别名
		var resp *anthropic.Message
		resp, upstreamErr = p.clients.AnthMessage(ctx, tpl, cred, params)
		if upstreamErr == nil {
			data, upstreamErr = json.Marshal(resp)
			if resp.JSON.Usage.Valid() {
				it, ot, tt, cr, cc = anthropicUsageFromResponse(resp.Usage)
			}
		}
	}
	if upstreamErr != nil {
		return statusOf(upstreamErr), upstreamBody(upstreamErr), false, upstreamErr
	}
	conv, err := protoconv.ConvertResponse(data, c.dir)
	if err != nil {
		// The upstream request has already completed. Returning handled=true is
		// intentional: retrying through failover could charge the same prompt a
		// second time. Record the conversion/application failure against the exact
		// selection and expose a stable 502 to the client.
		msg := domain.TruncateErrMsg("protocol response conversion failed: " + err.Error())
		l := logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, http.StatusBadGateway, domain.Err5xx, convertedUsageTuple(target, it, ot, tt, cr, cc), start))
		l.ErrorMessage = &msg
		p.finishSelection(sel, l)
		p.sched.MarkSelectionResult(sel, rule.Kind5xx, nil, http.StatusBadGateway, msg, sel.Model)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}})
		return http.StatusBadGateway, nil, true, nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(conv)
	p.sched.MarkSelectionResult(sel, rule.KindOK, nil, http.StatusOK, "", sel.Model)
	p.finishSelection(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, client, 200, domain.ErrNone, convertedUsageTuple(target, it, ot, tt, cr, cc), start)))
	return 200, nil, true, nil
}

// convertedUsageTuple preserves the source protocol's cache-total semantics
// while the usage log itself remains in the client's format. Anthropic excludes
// cache reads from input_tokens/total_tokens; OpenAI-family usage includes them.
func convertedUsageTuple(target domain.RequestFormat, it, ot, tt, cr, cc int64) usageTuple {
	u := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc}
	if target == domain.FormatAnthropic {
		u.cacheReadExcludedFromTotal = true
	} else {
		u.cacheReadIncludedInTotal = true
	}
	return u
}

// clientAndTargetOf 转换方向的客户端/模板协议格式（方向合法性由 W1 枚举校验
// 保证；未知方向 → 客户端=模板=零值，仅防御）。
func clientAndTargetOf(dir domain.ProtocolConvert) (domain.RequestFormat, domain.RequestFormat) {
	switch dir {
	case domain.ProtocolConvertChatToResp:
		return domain.FormatOpenAIChat, domain.FormatOpenAIResponses
	case domain.ProtocolConvertMessToResp:
		return domain.FormatAnthropic, domain.FormatOpenAIResponses
	case domain.ProtocolConvertRespToMess:
		return domain.FormatOpenAIResponses, domain.FormatAnthropic
	case domain.ProtocolConvertChatToMess:
		return domain.FormatOpenAIChat, domain.FormatAnthropic
	case protoconv.AutoResponsesToChat:
		return domain.FormatOpenAIResponses, domain.FormatOpenAIChat
	case protoconv.AutoMessagesToChat:
		return domain.FormatAnthropic, domain.FormatOpenAIChat
	}
	return "", ""
}
