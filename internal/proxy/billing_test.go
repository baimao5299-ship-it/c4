package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/billing"
	"go-proxy-mini/internal/credential"
	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/usage"
)

// proxyPricing 测试价格行：gpt-4o 基础价 $100/$200 每 1M（1e7/2e7 毫分），
// priority 档 $200/$300，fast ×2.0。非流式上游返回 usage 3/5 tokens →
// auto 130 毫分 / priority 210 / fast 260。
func proxyPricing() *domain.Pricing {
	i64 := func(v int64) *int64 { return &v }
	return &domain.Pricing{
		Model:                             "gpt-4o",
		PromptPricePerMillion:             1e7,
		CompletionPricePerMillion:         2e7,
		PriorityPromptPricePerMillion:     i64(2e7),
		PriorityCompletionPricePerMillion: i64(3e7),
		FastMultiplier:                    i64(20000), // ×2.0
	}
}

// fakePriceLookup 内存价格快照（proxy 计费测试用）。failFrom > 0 = 第 N 次
// GetPrice 调用起恒失败（模拟预检后快照被删竞态）。
type fakePriceLookup struct {
	mu       sync.Mutex
	m        map[string]*domain.Pricing
	call     int
	failFrom int
}

func (f *fakePriceLookup) GetPrice(model string) (*domain.Pricing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call++
	if f.failFrom > 0 && f.call >= f.failFrom {
		return nil, errors.New("no price")
	}
	if p, ok := f.m[model]; ok {
		return p, nil
	}
	return nil, errors.New("no price")
}

// newTestProxyBillingLogs 构造注入计费钩子的测试代理（默认 gpt-4o 模板 + 捕获
// 日志；policy nil = 恒透传）。Balances 空快照 → 倍率默认 ×1（T2 断言恒等，
// T3.5 无 nil 容忍：hooks 四字段齐备）。
func newTestProxyBillingLogs(t *testing.T, upstream string, prices *fakePriceLookup, policy func(billing.Tier) billing.TierPolicyMode, logs usage.LogInserter) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	return newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, logs, &BillingHooks{
		Prices:     prices,
		Balances:   billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
		TierPolicy: policy,
	})
}

// TestProxyBillingNoPrice402 缺价预检：计费启用且模型无价格 → 402 + 释放并发槽
// + 记 ErrBilling，上游一个请求都不许收到（评审 I-1：先 Release 再记录）。
func TestProxyBillingNoPrice402(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{}}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, http.StatusPaymentRequired, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "no price", "402 文案说明缺价")
	require.Zero(t, hits.Load(), "缺价不得转发上游")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "402 路径必须释放并发槽")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrBilling, store.logs[0].ErrorType)
	require.Equal(t, http.StatusPaymentRequired, store.logs[0].StatusCode)
	require.Zero(t, store.logs[0].Cost, "缺价拒绝 cost 0")
}

// TestProxyBillingAppliesCost finish applyBilling：成功请求按 tokens 计算毫分
// 成本（BillingTier=auto，无 service_tier）。
func TestProxyBillingAppliesCost(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "auto", store.logs[0].BillingTier, "无 service_tier → auto")
	require.Equal(t, int64(130), store.logs[0].Cost, "3×1e7+5×2e7 → 130 毫分")
	require.False(t, store.logs[0].AboveHit)
}

// TestProxyBillingTierPriority service_tier=priority：BillingTier 归一化落日志，
// 成本按 priority 单价档计算（210 ≠ auto 130）。
func TestProxyBillingTierPriority(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"priority","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "priority", store.logs[0].BillingTier)
	require.Equal(t, int64(210), store.logs[0].Cost, "priority 单价档：3×2e7+5×3e7 → 210 毫分")
}

// TestProxyBillingTierFast service_tier=fast：独立档位（Anthropic Fast Mode）→
// 整单 × fast_multiplier（130×2.0 = 260）。
func TestProxyBillingTierFast(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"fast","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "fast", store.logs[0].BillingTier)
	require.Equal(t, int64(260), store.logs[0].Cost, "fast ×2.0：130×2 = 260 毫分")
}

// TestProxyBillingTierPolicyStrip strip 策略：转发体删除 service_tier 字段（流式
// 原始 body 直发，可观测）；剥离路径计费照常（tier 已提取 → priority 单价）。
func TestProxyBillingTierPolicyStrip(t *testing.T) {
	gotTier := make(chan bool, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		_, hasTier := body["service_tier"]
		gotTier <- hasTier
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		func(billing.Tier) billing.TierPolicyMode { return billing.TierPolicyStrip }, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"priority","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.False(t, <-gotTier, "strip 策略：上游不得收到 service_tier 字段")

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "priority", store.logs[0].BillingTier, "剥离路径计费照常（tier 已提取）")
	require.Equal(t, int64(210), store.logs[0].Cost, "剥离路径按 priority 单价计费")
}

// TestProxyBillingTierPolicyReject reject 策略：直接 400 + 记 ErrBilling，
// 不转发上游；日志保留归一化 tier。
func TestProxyBillingTierPolicyReject(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		func(billing.Tier) billing.TierPolicyMode { return billing.TierPolicyReject }, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"priority","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Zero(t, hits.Load(), "reject 不得转发上游")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "reject 路径并发槽必须释放（acquire defer）")

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrBilling, store.logs[0].ErrorType)
	require.Equal(t, http.StatusBadRequest, store.logs[0].StatusCode)
	require.Equal(t, "priority", store.logs[0].BillingTier, "reject 日志保留归一化 tier")
}

// TestProxyBillingNoPriceDefenseAtFinish 运行时防御：预检通过后快照被删（竞态）→
// applyBilling Warn + BillingTier="no_price" + cost 0（不按 0 计价也不炸）。
func TestProxyBillingNoPriceDefenseAtFinish(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	// failFrom=2：预检（第 1 次）成功，finish applyBilling（第 2 次）失败。
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{
		m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}, failFrom: 2,
	}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String(), "预检通过 → 正常转发")

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "no_price", store.logs[0].BillingTier, "竞态防御：no_price 审计")
	require.Zero(t, store.logs[0].Cost, "缺价防御 cost 0")
}

// TestProxyBillingStreamAbortCostsTokens recordStreamAbort 修复（评审 M-2）：
// 上游停滞前已收到的 usage 帧必须参与计费（此前传 nil → tokens 全 0 → 消费不扣费）。
func TestProxyBillingStreamAbortCostsTokens(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`+"\n\n")
		fl.Flush()
		<-r.Context().Done() // 首帧后停滞 → UpstreamStreamTimeout 触发中止
	}))
	defer up.Close()
	store := &captureLogStore{}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 100*time.Millisecond, store, &BillingHooks{
		Prices:   &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "停滞超时记 ResultError")
	require.Zero(t, ri.Concurrency)
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType)
	require.Equal(t, int64(5), store.logs[0].PromptTokens, "中止前已累积的 usage 帧不丢")
	require.Equal(t, int64(7), store.logs[0].CompletionTokens)
	require.Equal(t, int64(190), store.logs[0].Cost, "5×1e7+7×2e7 → 190 毫分（计费不丢）")
}

// TestProxyBillingStreamAbortGroupMultiplier 评审 M-1：recordStreamAbort 传
// groupID → 中止路径组倍率生效（此前硬编码 0 → 组查找恒 miss → 按 ×1 计费，
// 上浮倍率少收/折扣倍率多收）。组倍率 15000（gk-1 → groupID 10）：
// 190×15000/10000 = 285 毫分，与正常路径一致。
func TestProxyBillingStreamAbortGroupMultiplier(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`+"\n\n")
		fl.Flush()
		<-r.Context().Done() // 首帧后停滞 → UpstreamStreamTimeout 触发中止
	}))
	defer up.Close()
	store := &captureLogStore{}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 100*time.Millisecond, store, &BillingHooks{
		Prices: &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		Balances: func() *billing.Balances {
			bal := billing.NewBalances(fakeBalanceLoader{
				m: map[int64]int64{}, gm: map[int64]int{10: 15000}, // 组倍率 ×1.5（用户未设置）
			}, nil)
			require.NoError(t, bal.Reload(context.Background()), "倍率快照加载（组倍率进快照）")
			return bal
		}(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	p.sched.FlushRules()
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType)
	require.Equal(t, int64(285), store.logs[0].Cost, "中止路径组倍率生效：190×15000/10000 = 285 毫分")
}

// TestProxyBillingDisabledPassthrough 计费全关（bill nil）：service_tier 恒透传
// （不 402、不 reject、不剥离），BillingTier 不落日志（空 = 未计费路径）。
func TestProxyBillingDisabledPassthrough(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store) // bill nil

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"priority","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String(), "计费全关：无价格表也不 402")

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "", store.logs[0].BillingTier, "计费全关：BillingTier 空")
	require.Zero(t, store.logs[0].Cost)
}

// ---------------------------------------------------------------------------
// T3：余额预检 402 + shouldBill 路由切换
// ---------------------------------------------------------------------------

// fakeBalanceLoader 余额 + 倍率快照测试 loader（um/gm 缺省 = 空倍率表）。
type fakeBalanceLoader struct {
	m  map[int64]int64 // 余额
	um map[int64]int   // 用户倍率（仅已设置行）
	gm map[int64]int   // 组倍率
}

func (f fakeBalanceLoader) LoadBalances(ctx context.Context) (map[int64]int64, map[int64]int, error) {
	return f.m, f.um, nil
}

func (f fakeBalanceLoader) LoadGroupMultipliers(ctx context.Context) (map[int64]int, error) {
	return f.gm, nil
}

// fakeDeductWriter 记录 DeductAndLog 调用（T3 billed 路由断言）。
type fakeDeductWriter struct {
	mu    sync.Mutex
	calls []deductCall
}

type deductCall struct {
	userID, cost int64
	logs         []*domain.UsageLog
}

func (f *fakeDeductWriter) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, deductCall{userID: userID, cost: cost, logs: logs})
	return false, 900000, nil
}

// newTestProxyBillingT3Logs 构造注入完整计费钩子（Prices+Balances+Flusher）的
// 测试代理：BillingCapture 开（shouldBill 路由 + 余额预检生效）。
func newTestProxyBillingT3Logs(t *testing.T, upstream string, prices *fakePriceLookup, bal *billing.Balances, f *billing.Flusher, logs usage.LogInserter) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, logs, &BillingHooks{
		Prices: prices, Balances: bal, Flusher: f,
	})
	p.cfg.BillingCapture = true
	return p
}

// TestProxyBillingInsufficientBalance402 余额预检（评审 I-1 无槽位问题）：
// 快照 ≤0 或缺失 → 402 + 上游零命中 + ErrBilling 日志（billed 路由进 flusher，
// 不进 rec.logCh），预检在 Acquire 前不占用并发槽。
func TestProxyBillingInsufficientBalance402(t *testing.T) {
	cases := []struct {
		name string
		bal  *billing.Balances
	}{
		{"余额 0", billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 0}}, nil)},
		{"快照缺失", billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil)},
		// 评审 I-1：快照缺失 + 组倍率显式 ×1（非免费）→ 仍 402（免费放行只对
		// 有效倍率 0 生效；缺失且非免费 = 无余额记录，语义不变）。
		{"快照缺失 + 组倍率 10000", billing.NewBalances(fakeBalanceLoader{gm: map[int64]int{10: 10000}}, nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(500)
			}))
			defer up.Close()
			store := &captureLogStore{}
			rec := usage.New(usage.UsageConfig{
				BatchSize: 100, FlushInterval: time.Hour,
				StatsFlushInterval: time.Hour,
			}, store, noopStatStore{}, nil)
			require.NoError(t, c.bal.Reload(context.Background()), "快照加载（余额 0 / 空表）")
			writer := &fakeDeductWriter{}
			f := billing.NewFlusher(billing.FlushConfig{
				FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
			}, writer, rec, c.bal, nil)
			p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, c.bal, f, store)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
			req.Header.Set("Authorization", "Bearer gk-1")
			recw := httptest.NewRecorder()
			p.HandleChat(recw, req)

			require.Equal(t, http.StatusPaymentRequired, recw.Code, "body=%s", recw.Body.String())
			require.Contains(t, recw.Body.String(), "insufficient balance", "402 文案说明余额不足")
			require.Zero(t, hits.Load(), "预检拒绝不得转发上游")
			ri, ok := p.sched.Runtime(1)
			require.True(t, ok)
			require.Zero(t, ri.Concurrency, "预检在 Acquire 前：不占用并发槽")
			require.Zero(t, p.rec.Pending(), "billed 日志不进 rec.logCh")

			require.NoError(t, f.Close(context.Background()))
			writer.mu.Lock()
			defer writer.mu.Unlock()
			require.Len(t, writer.calls, 1)
			require.Equal(t, domain.ErrBilling, writer.calls[0].logs[0].ErrorType)
			require.Equal(t, http.StatusPaymentRequired, writer.calls[0].logs[0].StatusCode)
			require.Zero(t, writer.calls[0].logs[0].Cost, "预检拒绝 cost 0")
		})
	}
}

// TestProxyBillingRoutesToFlusher shouldBill 路由切换（评审 C-4）：billed 日志
// 只进 flusher（rec.logCh 零残留——每日志恰好一个写者），扣费按聚合 cost 落库
// + 余额快照定向刷新。
func TestProxyBillingRoutesToFlusher(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		StatsFlushInterval: time.Hour,
	}, store, noopStatStore{}, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 50000}}, nil)
	require.NoError(t, bal.Reload(context.Background()), "快照加载（余额 50000）")
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String())
	require.Zero(t, p.rec.Pending(), "billed 日志不进 rec.logCh（每日志恰好一个写者）")

	require.NoError(t, f.Close(context.Background())) // 未 Start：排空 + 全量 flush
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Equal(t, int64(1), writer.calls[0].userID)
	require.Equal(t, int64(130), writer.calls[0].cost, "3×1e7+5×2e7 → 130 毫分")
	require.Len(t, writer.calls[0].logs, 1)
	require.Equal(t, int64(130), writer.calls[0].logs[0].Cost)
	require.Equal(t, int64(1), writer.calls[0].logs[0].UserID)
	got, ok := bal.BalanceOf(1)
	require.True(t, ok)
	require.Equal(t, int64(900000), got, "扣费成功 → 余额快照定向刷新")
}

// ---------------------------------------------------------------------------
// T3.5：价格倍率（applyMultiplier 纯函数 + 用户/组倍率应用 + 免费放行）
// ---------------------------------------------------------------------------

// TestApplyMultiplier 倍率纯函数表驱动：×2 上浮 / ×0.5 折扣 round（奇数 cost
// 验证四舍五入）/ 0 免费 / ×10 上限 / m==10000 恒等短路。
func TestApplyMultiplier(t *testing.T) {
	cases := []struct {
		name string
		cost int64
		m    int
		want int64
	}{
		{"×2 上浮", 130, 20000, 260},
		{"×1.5 精确", 130, 15000, 195},
		{"×0.5 折扣 half-up（131×0.5=65.5 → 66）", 131, 5000, 66},
		{"×0.5 折扣 half-up（129×0.5=64.5 → 65）", 129, 5000, 65},
		{"0 免费", 130, 0, 0},
		{"×10 上限", 130, 100000, 1300},
		{"m==10000 恒等", 130, 10000, 130},
		{"0 成本 × 倍率恒 0", 0, 20000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, applyMultiplier(c.cost, c.m))
		})
	}
}

// TestProxyBillingMultiplierUser 用户专属倍率（T3.5 用户覆盖组）：×2 → 扣费
// cost 翻倍（130×2 = 260），billed 路由 + 快照刷新照常。
func TestProxyBillingMultiplierUser(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		StatsFlushInterval: time.Hour,
	}, store, noopStatStore{}, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 50000}, um: map[int64]int{1: 20000},
	}, nil)
	require.NoError(t, bal.Reload(context.Background()), "快照加载（余额 50000 + 用户倍率 ×2）")
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String())

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Equal(t, int64(260), writer.calls[0].cost, "用户倍率 ×2：130×2 = 260 毫分")
	require.Equal(t, int64(260), writer.calls[0].logs[0].Cost)
}

// TestProxyBillingMultiplierGroup 组倍率（用户未设置 → 用组倍率）：×1.5 →
// 130×15000/10000 = 195；用户未设置不落入用户覆盖分支。
func TestProxyBillingMultiplierGroup(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		StatsFlushInterval: time.Hour,
	}, store, noopStatStore{}, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 50000}, gm: map[int64]int{10: 15000}, // gk-1 → groupID 10
	}, nil)
	require.NoError(t, bal.Reload(context.Background()), "快照加载（余额 50000 + 组倍率 ×1.5）")
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String())

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Equal(t, int64(195), writer.calls[0].cost, "组倍率 ×1.5：130×15000/10000 = 195 毫分")
}

// TestProxyBillingFreeUserPasses 免费用户放行（T3.5）：有效倍率 0 → 余额 0
// 不 402——正常转发，cost 0（只记日志不扣费）。
func TestProxyBillingFreeUserPasses(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		StatsFlushInterval: time.Hour,
	}, store, noopStatStore{}, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 0}, um: map[int64]int{1: 0}, // 余额 0 + 用户倍率 0（免费）
	}, nil)
	require.NoError(t, bal.Reload(context.Background()), "快照加载（余额 0 + 免费倍率）")
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String(), "免费用户余额 0 不 402")
	require.Zero(t, p.rec.Pending(), "billed 日志不进 rec.logCh")

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Zero(t, writer.calls[0].cost, "免费：cost 0 不扣费")
	require.Zero(t, writer.calls[0].logs[0].Cost)
	require.Equal(t, int64(1), writer.calls[0].logs[0].UserID)
}

// TestProxyBillingFreeGroupPasses 免费组放行（T3.5）：组倍率 0（用户未设置）
// → 余额 0 放行；与用户免费同判定（EffectiveMultiplier 共用）。
func TestProxyBillingFreeGroupPasses(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		StatsFlushInterval: time.Hour,
	}, store, noopStatStore{}, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 0}, gm: map[int64]int{10: 0}, // gk-1 → groupID 10；组免费
	}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String(), "免费组余额 0 不 402")

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Zero(t, writer.calls[0].cost, "免费组：cost 0 不扣费")
}

// TestProxyBillingFreeGroupSnapshotMissing 评审 I-1：快照缺失（Reload 滞后
// 窗口内用户无余额记录）但组免费（倍率 0）→ 放行不 402（此前只在 BalanceOf
// 命中时查倍率 → 免费组误 402）。缺失且非免费仍 402（见
// TestProxyBillingInsufficientBalance402）。
func TestProxyBillingFreeGroupSnapshotMissing(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		StatsFlushInterval: time.Hour,
	}, store, noopStatStore{}, nil)
	writer := &fakeDeductWriter{}
	// 余额快照为空（用户 1 不在快照）+ 组免费（gk-1 → groupID 10）。
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{}, gm: map[int64]int{10: 0},
	}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String(), "快照缺失窗口内免费组不 402")

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Zero(t, writer.calls[0].cost, "免费组：cost 0 不扣费")
	require.Zero(t, writer.calls[0].logs[0].Cost)
}

// TestProxyBillingNewUserImmediatelyUsable 评审 M-2 回归：新建用户（store 插入）
// → 全量 Reload → 立即请求 → 200（不得 402）。O1 前 Set 兜底补入新用户掩盖了
// 该窗口；O1 后 Set 仅限已存在条目（缺失忽略）——新用户必须经 Reload 进快照
//（创建路径不走 Set）。窗口显式暴露：创建前快照缺失 → 402（不用 sleep 掩盖）。
func TestProxyBillingNewUserImmediatelyUsable(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		StatsFlushInterval: time.Hour,
	}, store, noopStatStore{}, nil)
	writer := &fakeDeductWriter{}
	loader := &fakeBalanceLoader{m: map[int64]int64{}} // 用户 1 尚未创建
	bal := billing.NewBalances(loader, nil)
	require.NoError(t, bal.Reload(context.Background()))
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, store)

	req := func() int {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
		r.Header.Set("Authorization", "Bearer gk-1")
		rr := httptest.NewRecorder()
		p.HandleChat(rr, r)
		return rr.Code
	}
	// 窗口显式暴露：新用户未入快照 → 402（不得 sleep 掩盖）
	require.Equal(t, http.StatusPaymentRequired, req(), "创建前快照缺失 → 402 窗口如实暴露")

	// 用户创建 → invalidate → 全量 Reload（创建路径不走 Set）
	loader.m[1] = 50000
	require.NoError(t, bal.Reload(context.Background()))

	require.Equal(t, 200, req(), "新建用户 Reload 后立即请求不得 402（评审 M-2）")
	require.NoError(t, f.Close(context.Background()))
}
