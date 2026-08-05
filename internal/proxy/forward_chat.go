package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
	"go-proxy-mini/pkg/logx"
)

func (p *Proxy) HandleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := uuid.NewString()
	groupID, ok := p.auth.Authenticate(r)
	if !ok {
		writeErr(w, errInvalidKey)
		p.record(reqID, 0, 0, "", domain.FormatOpenAIChat, 401, domain.ErrAuth, 0, nil, start)
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
		p.record(reqID, groupID, 0, model, domain.FormatOpenAIChat, statusFor(err), domain.ErrNoAccount, 0, nil, start)
		return
	}

	var (
		lastCode int
		lastSel  = sel // 最后一次实际尝试的 Selection；中途 Select 失败返回 nil 时不得解引用 sel
	)
	for attempt := 0; attempt < p.cfg.FailoverAttempts; attempt++ {
		lastSel = sel
		ok, code, body := p.tryChat(w, r, reqID, groupID, start, sel, &params, peek.Stream)
		if ok {
			return // 已写出完整响应
		}
		lastCode = code
		if code == http.StatusTooManyRequests {
			p.sched.MarkResult(sel.AccountID, scheduler.Result429, nil)
		} else if code >= 500 || code == 0 {
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
		} else {
			// 4xx 确定性错误：透传上游状态码与原始 body，不转移（规格 §5.3）；
			// body 不可得（连接级错误不会有 4xx 码）才回退网关文案。
			p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, sel.Model, domain.FormatOpenAIChat, code, domain.Err4xx, nil, start))
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
	p.record(reqID, groupID, lastSel.AccountID, lastSel.Model, domain.FormatOpenAIChat, lastCode, et, 0, nil, start)
	if lastCode == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	} else {
		writeErr(w, &formatError{status: http.StatusBadGateway, msg: "all upstream attempts failed"})
	}
}

// tryChat 返回 (已完整处理, 上游状态码, 上游错误 body)。流式 200 发出后无法转移。
func (p *Proxy) tryChat(w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, params *openai.ChatCompletionNewParams, streaming bool) (bool, int, []byte) {
	tpl := tplOf(sel)
	params.Model = sel.Model

	if streaming {
		ctx, cancel := context.WithTimeout(r.Context(), p.cfg.UpstreamStreamTimeout)
		defer cancel()
		stream := p.clients.ChatCompletionStream(ctx, tpl, sel.UpstreamKey, *params)
		if stream.Err() != nil {
			err := stream.Err()
			return false, statusOf(err), upstreamBody(err)
		}
		sw := newSSEWriter(w)
		var (
			usage    openai.CompletionUsage
			hasUsage bool
		)
		for stream.Next() {
			chunk := stream.Current()
			if err := sw.Event(chunk); err != nil {
				sw.Abort()
				_ = stream.Close() // 归还上游连接
				if p.log != nil {
					p.log.Warn("sse write failed", logx.String("request_id", reqID), logx.Error(err))
				}
				p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
				p.finish(sel.AccountID, nil) // 客户端断开，无法转移
				return true, 0, nil
			}
			if chunk.JSON.Usage.Valid() {
				usage = chunk.Usage
				hasUsage = true
			}
		}
		if err := stream.Err(); err != nil {
			sw.Abort()
			_ = stream.Close()
			p.recordStreamAbort(reqID, start, sel, err)
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil)
			return true, 0, nil
		}
		_ = stream.Close()
		_ = sw.Done()
		var pt, ct, tt int64
		if hasUsage {
			pt, ct, tt = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
		}
		p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil)
		p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, sel.Model, domain.FormatOpenAIChat, 200, domain.ErrNone, &usageTuple{pt, ct, tt}, start))
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
	var pt, ct, tt int64
	if resp.JSON.Usage.Valid() {
		pt, ct, tt = resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil)
	p.finish(sel.AccountID, p.buildLog(reqID, groupID, sel.AccountID, sel.Model, domain.FormatOpenAIChat, 200, domain.ErrNone, &usageTuple{pt, ct, tt}, start))
	return true, 200, nil
}
