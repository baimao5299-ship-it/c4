// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

// errCodexSearchNotIntegrated 501：codex 适配层未装配（SetCodex 未调用——main
// 装配缺失的显式拒绝，不让凭据缺失路径误报 502/network；与 images/resp 路径
// 同款）。
var errCodexSearchNotIntegrated = &formatError{status: http.StatusNotImplemented, msg: "codex search unavailable (adapter not wired)"}

// HandleSearch 转发 codex /v1/alpha/search（spec 2026-08-13 v2）：codex CLI 以
// 独立 unary POST 调 web search（模型发 web.run tool call 时触发，与主
// /responses 流并发）。**透传语义：请求体/响应体原样**（opaque results/
// encrypted_output 网关零解析——alpha 端点实验性，上游变更网关免疫）。
//
// 与主 handleFormat 的差异（search 专属语义，spec 边界声明）：
//   - 账号选择：body.model → Scheduler.Select(groupID, openai-responses, model)
//     （复用主流 resp 路由面——四类型全可达；**独立选号无会话绑定**——P2 裁
//     决：search 请求自包含，上游鉴权 = 有效 Bearer，无会话亲和机制）
//   - **不走计费预检**（余额/缺价 402 均不执行——search 无预检语义；按次价在
//     2xx 落账时结算，零余额透支扣费为产品语义，防实现期误当缺陷"修复"）
//   - **四类型分派（用户裁决 2026-08-13）**：codex-oauth/codex-pat → codex-sdk
//     Search（适配层 clientFor 缓存客户端直接复用——统一 client 形态；
//     search URL 由 SDK 方法内派生，网关零拼装；Auth 注入/刷新/fatal 生命周期
//     复用既有 SDK 面）；api_key/responses-special → 静态透传（Bearer upstream
//     key 直连上游——aiclient 既有静态 key 通道零新增机制；URL 裸根派生
//     base/v1/alpha/search）。组内混合类型路由允许（任一类型均可用——不再本地
//     拒绝）
//   - **x-codex-turn-metadata 统一不转发**（两路径均不带上游——SDK 默认头面
//     无该头；静态 rawPostCT 构造全新 Header 只设 Content-Type + Authorization，
//     与主流静态路径现状一致）
//   - **不做 ModelMapping 改写（P3-3 显式取舍）**：请求体原样 = 映射对 search
//     不生效（上游收客户端模型名）——零解析是 spec 显式约束，自洽记录
//   - 计费：2xx → usage_logs 行（format=openai-search + call_count=1 +
//     price_per_call_millis=GetFunctionPrice("codex-search") + cost=按次价×整单
//     倍率，applyBilling search 分支）；非 2xx/网络错误 → 不计费（cost=0，错误
//     行走既有 err_logs 面）
//
// 复用面（评审 P3-4 点名）：鉴权/配额/并发门禁/限流序列、Select +
// handleSelectError、信封分类（statusOf/upstreamBody）、failover 循环（**每轮
// 按当轮 sel.CredentialType 重新分派**——对齐 caller.go:299-309 P1-1 教训：
// 跨类型换账号复用旧调用器会把健康账号路由到错误凭据路径）、
// recordRejected/finish/buildLog/MarkResult 全部既有机制零改动。
func (p *Proxy) HandleSearch(w http.ResponseWriter, r *http.Request) {
	p.inflight.Add(1) // 优雅停机等在途归零（main waitForInflight 轮询 Inflight()）
	defer p.inflight.Add(-1)
	start := time.Now()
	reqID := newReqID()
	meta, ok := p.auth.Authenticate(r)
	if !ok {
		writeErr(w, errInvalidKey)
		p.recordRejected(r.Context(), reqID, 0, 0, "", "", domain.FormatOpenAISearch, http.StatusUnauthorized, domain.ErrAuth, 0, usageTuple{}, start, errInvalidKey.msg)
		return
	}
	groupID := meta.GroupID
	// 请求元数据入 context（user_id/key_id 日志归属；与 handleFormat 同款单键
	// 单值 + 指针原地补 tier——GC 削减 P6 语义）。
	rm := &reqMeta{meta: meta}
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyReqMeta{}, rm))

	// quota 检查在并发 acquire 之前（失败无并发槽副作用；同 handleFormat 序列）。
	if p.auth.QuotaExhausted(meta) {
		writeErr(w, errQuotaExhausted)
		p.recordRejected(r.Context(), reqID, groupID, 0, "", "", domain.FormatOpenAISearch, http.StatusTooManyRequests, domain.Err429, 0, usageTuple{}, start, errQuotaExhausted.msg)
		return
	}
	// 无余额预检（search 无 402 语义——见函数头注释）。
	acquired, ok := p.auth.Acquire(meta)
	if !ok {
		writeErr(w, errConcurrency)
		p.recordRejected(r.Context(), reqID, groupID, 0, "", "", domain.FormatOpenAISearch, http.StatusTooManyRequests, domain.Err429, 0, usageTuple{}, start, errConcurrency.msg)
		return
	}
	defer p.auth.Release(meta, acquired)

	if !p.limit.Allow(groupID, time.Now()) {
		writeErr(w, errRateLimit)
		p.recordRejected(r.Context(), reqID, groupID, 0, "", "", domain.FormatOpenAISearch, http.StatusTooManyRequests, domain.Err429, 0, usageTuple{}, start, errRateLimit.msg)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.cfg.MaxBodySize))
	if err != nil {
		writeErr(w, errBody)
		return
	}
	// 本地 400（同 handleFormat 语义：json.Valid 硬门零分配；search 无 stream/
	// service_tier 面——body 原样透传，不做任何改写）。
	if !json.Valid(body) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: invalid JSON"}})
		return
	}
	// 客户端请求模型（日志口径 + 选号）：gjson 顶层提取（1 次分配，与 resp
	// 流式路径同款）。model 缺失/非法 → 空串回落默认桶（Select 既有语义——
	// 上游契约必填 id/model，缺失由上游 4xx 兜底，网关零新增校验）。
	reqModel := gjson.GetBytes(body, "model").String()

	// 选号：复用主流 resp 路由面（openai-responses 格式——四类型全可达；
	// search 无独立路由，独立选号无会话绑定）。
	sel, err := p.sched.Select(groupID, domain.FormatOpenAIResponses, reqModel)
	if err != nil {
		p.handleSelectError(w, err)
		p.recordRejected(r.Context(), reqID, groupID, 0, reqModel, "", domain.FormatOpenAISearch, statusFor(err), domain.ErrNoAccount, 0, usageTuple{}, start, selectErrorMessage(err))
		return
	}

	var (
		lastCode   int
		lastErrMsg string // 最后一次实际尝试的错误文本（耗尽路径 ErrorMessage 用）
		lastSel    = sel  // 最后一次实际尝试的 Selection；中途 Select 失败返回 nil 时不得解引用 sel
	)
	for attempt := 0; attempt < p.cfg.FailoverAttempts; attempt++ {
		lastSel = sel
		code, respBody, handled, callErr := p.callSearch(r.Context(), w, r, reqID, groupID, start, sel, reqModel, body)
		if handled {
			return // 已处理完毕（成功已记录 / 501 本地拒绝已写出）
		}
		lastCode = code
		if code == http.StatusTooManyRequests {
			// 429：上游 body message（既有语义；域内截断 500）
			lastErrMsg = domain.TruncateErrMsg(upstreamErrMsg(respBody))
			p.sched.MarkResult(sel.AccountID, scheduler.Result429, nil, code, lastErrMsg)
		} else if code >= 500 || code == 0 {
			// 首字节前客户端断连（分类正确性同 handleFormat）：r.Context() 已取
			// 消 → SDK 返回 context.Canceled（statusOf=0）——客户端行为非上游错
			// 误：不 failover、不 MarkResult/冷却；记 499 + ErrAbort 立即返回。
			if code == 0 && r.Context().Err() != nil {
				l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAISearch, statusClientClosedRequest, domain.ErrAbort, usageTuple{}, start))
				msg := "client closed request before upstream response"
				l.ErrorMessage = &msg
				p.finish(sel.AccountID, l)
				return
			}
			// 5xx：上游 body message。连接级/凭据错（code==0）：err.Error() 全文
			// 填 last_error 与耗尽记录（域内截断 500），并附加 Warn（全文留痕）。
			lastErrMsg = upstreamErrMsg(respBody)
			if code == 0 && callErr != nil {
				lastErrMsg = domain.TruncateErrMsg(callErr.Error())
				if p.log != nil {
					p.log.Warn("upstream search connection failure",
						logx.String("request_id", reqID),
						logx.Int64("account_id", sel.AccountID),
						logx.String("model", sel.Model),
						logx.Error(callErr))
				}
			} else if lastErrMsg != "" {
				lastErrMsg = domain.TruncateErrMsg(lastErrMsg)
			}
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, code, lastErrMsg)
		} else {
			// 4xx 确定性错误：透传上游状态码与原始 body，不转移（规格 §5.3）；
			// **不计费**（cost=0，Err4xx 走 err_logs 面——routeLog 失败行语义）。
			l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAISearch, code, domain.Err4xx, usageTuple{}, start))
			if em := domain.TruncateErrMsg(string(respBody)); em != "" {
				l.ErrorMessage = &em
			}
			p.finish(sel.AccountID, l)
			if len(respBody) > 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = w.Write(respBody)
			} else {
				writeJSON(w, code, map[string]any{"error": map[string]any{
					"message": "upstream rejected request", "type": "upstream_error",
				}})
			}
			return
		}
		p.sched.Release(sel.AccountID)
		// 最后一轮不再为不存在的下一次尝试预选（尾部 Select 抢占并发槽泄漏——
		// 与 handleFormat 同款注释语义）。
		if attempt+1 >= p.cfg.FailoverAttempts {
			break
		}
		sel, err = p.sched.Select(groupID, domain.FormatOpenAIResponses, reqModel)
		if err != nil {
			break
		}
	}
	// 耗尽：请求已完成（上游消费了请求），以最后一次尝试的结果记一条用量；
	// **不计费**（cost=0，错误行走 err_logs 面）。
	et := domain.Err5xx
	switch {
	case lastCode == http.StatusTooManyRequests:
		et = domain.Err429
	case lastCode == 0:
		et = domain.ErrNetwork
	}
	l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, lastSel.AccountID, reqModel, lastSel.Model, domain.FormatOpenAISearch, lastCode, et, usageTuple{}, start))
	if lastErrMsg != "" {
		l.ErrorMessage = &lastErrMsg
	}
	p.recordLog(l)
	if lastCode == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	} else {
		writeErr(w, &formatError{status: http.StatusBadGateway, msg: "all upstream attempts failed"})
	}
}

// callSearch 单次 codex search 上游调用（非流式 unary 透传；**四类型分派**——
// 用户裁决 2026-08-13）：按当轮 sel.CredentialType 路由——
//   - codex-oauth/codex-pat → callCodexSearch（适配层 SDK Search——统一 client
//     形态直接复用；Auth 注入 + fatal 生命周期复用既有 SDK 面）
//   - api_key/responses-special → callStaticSearch（Bearer upstream key 直连
//     上游——aiclient 既有静态 key 通道）
//
// 分派在 callSearch 内每轮重新执行（P1-1 教训——跨类型换账号按新类型走新路
// 径，不缓存调用器）。
func (p *Proxy) callSearch(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, body []byte) (int, []byte, bool, error) {
	if isCodexCredentialType(sel.CredentialType) {
		return p.callCodexSearch(ctx, w, r, reqID, groupID, start, sel, reqModel, body)
	}
	return p.callStaticSearch(ctx, w, r, reqID, groupID, start, sel, reqModel, body)
}

// callCodexSearch codex-oauth/codex-pat 类型 search 调用（SDK 路径）：凭据线
// 快照派生直供适配层（与 resp 路径同款——codex 凭据为复合结构，单字符串契约
// 表达不到）→ 适配层 Search（clientFor 缓存客户端 → e.client.Search——body
// 零改写、响应零解析）→ 2xx → 响应原样写出 + MarkResult + finish
// （usageTuple{calls:1} 落 CallCount——按次计费在 applyBilling 的 search 分支
// 结算）；非 2xx → 信封分类返回（4xx 透传 / 429/5xx failover，与 resp HTTP
// 分支同语义）。
//
//   - 适配层未装配（SetCodex nil）→ 501 显式拒绝（release + recordRejected +
//     writeErr，handled=true）
//   - 配置损坏（codex 账号缺 account_ext 快照）→ 连接级错误转移（handled=false
//     ——失败文本落盘，耗尽 502；不上报失效，与 resp/WS 路径 errCodexExtMissing
//     同语义）
func (p *Proxy) callCodexSearch(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, body []byte) (int, []byte, bool, error) {
	if p.codex == nil {
		// 适配层未装配（SetCodex 未调用）：显式 501（防 nil 误走凭据缺失 502）。
		p.sched.Release(sel.AccountID)
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAISearch, http.StatusNotImplemented, domain.ErrBilling, 0, usageTuple{}, start, errCodexSearchNotIntegrated.msg)
		writeErr(w, errCodexSearchNotIntegrated)
		return 0, nil, true, nil
	}
	if sel.Ext == nil {
		// 配置损坏（codex 账号必有 ext 行——快照缺 account_ext 行）：本地配置
		// 错误按连接级错误转移（失败文本落盘，耗尽 502 语义）；不上报失效（避
		// 免 account 0 无谓上报——与 resp/WS 路径 errCodexExtMissing 同语义）。
		return 0, nil, false, errCodexExtMissing
	}
	// 凭据线：快照派生直供适配层（与 resp/images 路径同款）。cred.BaseURL =
	// responses 完整端点（SDK Search 方法内按其尾段派生 /alpha/search——网关
	// 零拼装）。
	cred := domain.CredentialFromExt(sel.Ext)
	full, err := p.clients.ResponsesWSURL(sel.TemplateID, sel.BaseURL)
	if err != nil {
		return 0, nil, false, err
	}
	cred.BaseURL = full
	// 非流式超时（同 nonstreamCodexResponses 语义）：HTTPClient.Timeout 不可用
	// ——TCP 黑洞读停滞 → 超时触发 → 连接级错误转移（failover 可转移）。
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamTimeout)
	defer cancel()
	// 无头注入：x-codex-turn-metadata 统一不转发（SDK Search 默认头面无该头，
	// 与 resp HTTP 路径现状一致）。
	resp, err := p.codex.Search(ctx, &cred, body)
	if err != nil {
		return statusOf(err), upstreamBody(err), false, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Raw)
	// 2xx → 按次计费落账（call_count=1；price_per_call/cost 由 applyBilling
	// search 分支按 GetFunctionPrice("codex-search") 结算——无 token 分量）。
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAISearch, http.StatusOK, domain.ErrNone, usageTuple{calls: 1}, start)))
	return http.StatusOK, nil, true, nil
}

// callStaticSearch api_key/responses-special 类型 search 调用（静态透传路径）：
// 既有静态 key 通道（credentialFor → aiclient rawPost——Bearer upstream key
// 直连上游；URL 裸根派生 base/v1/alpha/search——与主流静态 responses 路径
// base/v1/responses 同款派生语义，尾段即 /alpha/search）。错误信封 = 原始
// HTTP 状态 + body 透传（caller_responses.go:65-68 先例——非 200 读取 body
// 交 failover 循环分类；SDK 路径的 translateError 信封不适用）。
//
// **无客户端头透传**（x-codex-turn-metadata 统一不转发——rawPostCT 构造全新
// Header 只设 Content-Type + Authorization，与主流静态路径现状一致）。
func (p *Proxy) callStaticSearch(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, body []byte) (int, []byte, bool, error) {
	cred, err := p.credentialFor(ctx, sel)
	if err != nil {
		return 0, nil, false, err // 凭据错误按连接级处理（耗尽 502，与既有路径同语义）
	}
	// 非流式超时（同 codex 路径语义）：TCP 黑洞读停滞 → 超时触发 → 连接级错误
	// 转移（failover 可转移）。
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamTimeout)
	defer cancel()
	resp, err := p.clients.SearchRaw(ctx, sel.TemplateID, sel.BaseURL, cred, body)
	if err != nil {
		return statusOf(err), upstreamBody(err), false, err
	}
	if resp.StatusCode != http.StatusOK {
		rb := readUpstreamBody(resp)
		resp.Body.Close()
		return resp.StatusCode, rb, false, nil
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return 0, nil, false, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	// 2xx → 按次计费落账（call_count=1；与 codex 路径同款——applyBilling
	// search 分支结算）。
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAISearch, http.StatusOK, domain.ErrNone, usageTuple{calls: 1}, start)))
	return http.StatusOK, nil, true, nil
}
