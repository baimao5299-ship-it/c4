package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"go-proxy-mini/internal/billing"
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/scheduler"
)

// UpstreamCaller 一格式一实现：完成单次上游调用（含流式写出、客户端断开判定
// 与 usage 记录）。记录职责全在 caller（finish/buildLog/recordStreamAbort/
// MarkResult 直接可用——评审 I-1）；骨架只做 code 分支（429/5xx 转移、4xx
// 透传记录）、handled 短路与耗尽 record。凭据值经 aiclient 格式方法传入
// （头名 aiclient 内组装，Phase 1 正交延续——评审 M-2）。
//
// 语义：
//   - handled == true → 请求已处理完毕（成功/客户端断开/流中止已记录；本地拒绝
//     已写出无记录），骨架直接 return（不可转移）
//   - handled == false → 上游未接受，骨架接手：
//     code 429 → MarkResult(Result429) + Release + 转移
//     code >= 500 或 code == 0（连接级/凭据错）→ MarkResult(ResultError) + Release + 转移
//     code 4xx（err == nil）→ 骨架 finish(buildLog(Err4xx)) + 透传 respBody
//     （空 → 网关文案 "upstream rejected request"）
type UpstreamCaller interface {
	Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64,
		start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (code int, respBody []byte, handled bool, err error)
}

// handleFormat 通用转发骨架（从原 HandleXxx 提取，三格式共用）：鉴权 →
// quota 检查（纯读）→ 两级并发门禁 acquire（user → key，失败回滚）→ 限流 →
// 读体 → stream 探测（peek unmarshal）+ model 提取（gjson，零分配）→ 选号 →
// failover 循环（每轮 credentialFor + caller.Call + code 分支）→ 耗尽记录写出。
// 门禁全部内存原子（零 DB 零锁）；release 与 quota 扣减在请求结束统一完成。
func (p *Proxy) handleFormat(format domain.RequestFormat, w http.ResponseWriter, r *http.Request) {
	p.inflight.Add(1) // 优雅停机等在途归零（main waitForInflight 轮询 Inflight()）
	defer p.inflight.Add(-1)
	start := time.Now()
	reqID := uuid.NewString()
	meta, ok := p.auth.Authenticate(r)
	if !ok {
		writeErr(w, errInvalidKey)
		p.record(r.Context(), reqID, 0, 0, "", "", format, 401, domain.ErrAuth, 0, nil, start)
		return
	}
	groupID := meta.GroupID
	// 鉴权元数据入 context（user_id/key_id 日志归属；不改变 Call/buildLog 签名）
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyMeta{}, meta))

	// quota 检查在并发 acquire 之前（评审提醒①：纯读失败无计数副作用；
	// 未设置额度 key 短路零成本）
	if p.auth.QuotaExhausted(meta) {
		writeErr(w, errQuotaExhausted)
		p.record(r.Context(), reqID, groupID, 0, "", "", format, http.StatusTooManyRequests, domain.Err429, 0, nil, start)
		return
	}
	// 余额预检（Phase 5 计费；评审 I-1 无槽位问题）：快照读零 DB（滞后 ≤
	// BalanceRefreshInterval，多实例条件扣 DB 兜底）。快照缺失或 ≤0 → 402
	// errInsufficientBalance（不按 0 记账），但免费放行（T3.5，评审 I-1 修复）：
	// 有效倍率 0 = 免费用户/组 → 缺失/0 余额不 402（与 applyBilling 同一快照
	// 同一判定；cost 0 只记日志不扣费）。快照缺失窗口内免费组照常放行；
	// 缺失且非免费 → 仍 402（用户不在快照 = 无余额记录，语义不变）。
	// 在 Acquire 前 → 不占用并发槽。
	if p.cfg.BillingCapture && p.bill != nil {
		bal, ok := p.bill.Balances.BalanceOf(meta.UserID)
		if (!ok || bal <= 0) && p.bill.Balances.EffectiveMultiplier(meta.UserID, groupID) != 0 {
			writeErr(w, errInsufficientBalance)
			p.record(r.Context(), reqID, groupID, 0, "", "", format, http.StatusPaymentRequired, domain.ErrBilling, 0, nil, start)
			return
		}
	}
	// 两级并发门禁（user → key；两步回滚由 gate 内部完成；release 仅释放
	// 已 acquire 层级——defer 覆盖全部返回路径）
	acquired, ok := p.auth.Acquire(meta)
	if !ok {
		writeErr(w, errConcurrency)
		p.record(r.Context(), reqID, groupID, 0, "", "", format, http.StatusTooManyRequests, domain.Err429, 0, nil, start)
		return
	}
	defer p.auth.Release(meta, acquired)

	if !p.limit.Allow(groupID, time.Now()) {
		writeErr(w, errRateLimit)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.cfg.MaxBodySize))
	if err != nil {
		writeErr(w, errBody)
		return
	}
	// SDK v1.x 参数里没有 Stream 字段（流式由 NewStreaming 在请求选项层注入
	// "stream": true），故从原始请求体探测 stream 标志决定走流式还是非流式。
	// model 一并在此提取（评审 I-2：不解析完整 params）：string 类型字段让
	// 非字符串 model 在解码时报错 → 400；显式 null 与缺失等同（encoding/json
	// 的 null → 零值语义）。service_tier（Phase 5 计费）同一次 unmarshal 提取，
	// 零额外分配。与 gjson 顶层提取语义等价，但零额外分配（热路径
	// alloc/op 硬标准：与现状 peek 单次 unmarshal 相同）。
	var peek struct {
		Stream      bool   `json:"stream"`
		Model       string `json:"model"`
		ServiceTier string `json:"service_tier"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
		return
	}
	reqModel := peek.Model
	// service_tier 归一化 + 转发策略（计费启用才处理；auto/空/未知恒透传）：
	// strip → 转发体删该字段；reject → 直接 400（记 ErrBilling，不转发）。
	// 归一化 tier 先入 ctx（ctxKeyTier），剥离/拒绝路径计费读取照常。
	if p.bill != nil {
		tier := billing.NormalizeTier(peek.ServiceTier)
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyTier{}, tier))
		if (tier == billing.TierPriority || tier == billing.TierFlex) && p.bill.TierPolicy != nil {
			switch p.bill.TierPolicy(tier) {
			case billing.TierPolicyStrip:
				if body, err = stripServiceTier(body); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
					return
				}
			case billing.TierPolicyReject:
				writeErr(w, errServiceTierRejected)
				p.record(r.Context(), reqID, groupID, 0, reqModel, "", format, http.StatusBadRequest, domain.ErrBilling, 0, nil, start)
				return
			}
		}
	}

	sel, err := p.sched.Select(groupID, format, reqModel)
	if err != nil {
		p.handleSelectError(w, err)
		p.record(r.Context(), reqID, groupID, 0, reqModel, "", format, statusFor(err), domain.ErrNoAccount, 0, nil, start)
		return
	}

	// 注册表查找在 failover 循环外（评审 I-3）：格式固定，每轮不重查。
	caller := p.callers[format]

	var (
		lastCode int
		lastSel  = sel // 最后一次实际尝试的 Selection；中途 Select 失败返回 nil 时不得解引用 sel
	)
	for attempt := 0; attempt < p.cfg.FailoverAttempts; attempt++ {
		lastSel = sel
		// 缺价预检（评审 I-1）：每轮 sel 更新后、Call 前查价——计费启用时模型
		// 无价格 → 释放并发槽 + 402（不按 0 计价），零 DB（快照读）。
		if p.bill != nil && p.bill.Prices != nil {
			if _, err := p.bill.Prices.GetPrice(sel.Model); err != nil {
				p.sched.Release(sel.AccountID)
				p.record(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, format, http.StatusPaymentRequired, domain.ErrBilling, 0, nil, start)
				writeErr(w, errNoPrice)
				return
			}
		}
		// 凭据每轮取（评审 I-3）：尾部 Select 后 Selection 变化，凭据随账号；
		// 循环外取一次会把旧账号 key 发给新账号上游。
		cred, err := p.credentialFor(r.Context(), sel)
		var (
			code     int
			respBody []byte
			handled  bool
		)
		if err != nil {
			code = 0 // 凭据错误按网络错误处理（等价现状 try* 内 false,0,nil → 耗尽 ErrNetwork）
		} else {
			// err 返回值为接口契约保留（评审 I-1 语义表），实际分类已由
			// code 承载（0=连接级/凭据错、4xx、429、5xx），骨架无需 err。
			code, respBody, handled, _ = caller.Call(r.Context(), w, r, reqID, groupID, start, sel, cred, body, peek.Stream)
		}
		if handled {
			return // caller 已处理完毕（成功/客户端断开/流中止已记录；本地拒绝已写出无记录）
		}
		lastCode = code
		if code == http.StatusTooManyRequests {
			p.sched.MarkResult(sel.AccountID, scheduler.Result429, nil, code, upstreamErrMsg(respBody))
		} else if code >= 500 || code == 0 {
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, code, upstreamErrMsg(respBody))
		} else {
			// 4xx 确定性错误：透传上游状态码与原始 body，不转移（规格 §5.3）；
			// body 不可得（连接级错误不会有 4xx 码）才回退网关文案。
			p.finish(sel.AccountID, logWithCtx(r.Context(), p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, format, code, domain.Err4xx, nil, start)))
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
		// 最后一轮不再为不存在的下一次尝试预选：尾部 Select 会抢占并发槽
		// （CAS 递增、仅 Release 递减、无回收），耗尽时永不释放 → 永久占槽。
		if attempt+1 >= p.cfg.FailoverAttempts {
			break
		}
		var selErr error
		sel, selErr = p.sched.Select(groupID, format, reqModel)
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
	p.record(r.Context(), reqID, groupID, lastSel.AccountID, reqModel, lastSel.Model, format, lastCode, et, 0, nil, start)
	if lastCode == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	} else {
		writeErr(w, &formatError{status: http.StatusBadGateway, msg: "all upstream attempts failed"})
	}
}
