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
// 日志；policy nil = 恒透传）。
func newTestProxyBillingLogs(t *testing.T, upstream string, prices *fakePriceLookup, policy func(billing.Tier) billing.TierPolicyMode, logs usage.LogInserter) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	return newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, logs, &BillingHooks{
		Prices:     prices,
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
		Prices: &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
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
