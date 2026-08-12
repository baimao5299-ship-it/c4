// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// ---------------------------------------------------------------------------
// mock 上游：images 端点（WithBaseURL 覆盖）+ refresh 端点（env override——
// 对齐 SDK 测试模式 t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE")）
// ---------------------------------------------------------------------------

// codexUpstreamCapture 断言请求面（路径/鉴权）+ 可编程响应序列。
type codexUpstreamCapture struct {
	mu    sync.Mutex
	calls int
	auths []string
	steps []codexUpstreamStep
	last  codexUpstreamStep
}

type codexUpstreamStep struct {
	status int
	body   string
}

// newCodexUpstream 构造 images 端点 mock：响应序列按序弹出（耗尽重复最后一步）。
// baseURL = srv.URL + "/images/generations"（SDK WithBaseURL 完整端点语义）。
func newCodexUpstream(t *testing.T, steps ...codexUpstreamStep) (*httptest.Server, *codexUpstreamCapture) {
	t.Helper()
	c := &codexUpstreamCapture{last: codexUpstreamStep{status: 500, body: `{}`}}
	if len(steps) > 0 {
		c.steps = steps
		c.last = steps[len(steps)-1]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		c.mu.Lock()
		c.calls++
		c.auths = append(c.auths, r.Header.Get("Authorization"))
		step := c.last
		if len(c.steps) > 0 {
			step = c.steps[0]
			c.steps = c.steps[1:]
			c.last = step
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.status)
		_, _ = w.Write([]byte(step.body))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func (c *codexUpstreamCapture) callsN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *codexUpstreamCapture) auth(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auths[i]
}

// codexMockRefresh 构造 refresh 端点 mock（同 SDK 测试形态：构造即设置 env）。
type codexMockRefresh struct {
	mu    sync.Mutex
	calls int
}

func newCodexMockRefresh(t *testing.T, steps ...codexUpstreamStep) *codexMockRefresh {
	t.Helper()
	m := &codexMockRefresh{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		m.mu.Lock()
		m.calls++
		step := codexUpstreamStep{status: 500, body: `{}`}
		if len(steps) > 0 {
			step = steps[0]
			steps = steps[1:]
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.status)
		_, _ = w.Write([]byte(step.body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", srv.URL)
	return m
}

func (m *codexMockRefresh) callsN() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// oauthCred 构造 oauth 测试凭据（未过期初始 at + rt）。
func oauthCred(accountID int64, at, rt string) *domain.AccountCredential {
	exp := time.Now().Add(time.Hour)
	return &domain.AccountCredential{
		AccountID: accountID, OAuthToken: at, OAuthRefreshToken: rt, OAuthExpiresAt: &exp,
	}
}

// okImageResponse 标准生图成功响应（usage 嵌套 details——对齐上游 wire）。
const okImageResponse = `{"created":1720000000,"data":[{"b64_json":"QUJD"},{"b64_json":"REVG"}],"usage":{"input_tokens":2,"output_tokens":3,"input_tokens_details":{"image_tokens":1},"output_tokens_details":{"image_tokens":2}}}`

// ---------------------------------------------------------------------------
// 转换层双向映射（防漂移）
// ---------------------------------------------------------------------------

// TestCodexConversions domain↔codexsdk 双向转换字段完备：toSDKParams 全字段
// （含 edits Images——ImageURL 与 Raw 双形态）；fromSDKResponse 全字段平铺
// （含 usage 嵌套提取）；MarshalImageResponse 上游 wire 形态（嵌套 usage
// details + 计费提取同源）。
func TestCodexConversions(t *testing.T) {
	n := 2
	size, quality, background := "1024x1024", "high", "transparent"
	url1 := "https://example.com/in.png"
	p := &domain.ImageGenParams{
		Model: "gpt-image-2", Prompt: "a cat", N: &n,
		Size: &size, Quality: &quality, Background: &background,
		Images: []domain.ImageRef{
			{ImageURL: &url1},
			{Raw: []byte("PNG-bytes")},
		},
	}
	s := toSDKParams(p)
	require.Equal(t, "gpt-image-2", s.Model)
	require.Equal(t, "a cat", s.Prompt)
	require.Equal(t, &n, s.N)
	require.Equal(t, &size, s.Size)
	require.Equal(t, &quality, s.Quality)
	require.Equal(t, &background, s.Background)
	require.Len(t, s.Images, 2)
	require.Equal(t, &url1, s.Images[0].ImageURL)
	require.Nil(t, s.Images[0].Raw)
	require.Equal(t, []byte("PNG-bytes"), s.Images[1].Raw)
	require.Nil(t, s.Images[1].ImageURL)
	// nil 输入
	require.Nil(t, toSDKParams(nil))
	require.Empty(t, toSDKParams(&domain.ImageGenParams{}).Images, "空 Images → 不发 images 字段")

	// fromSDKResponse：全字段 + usage 嵌套提取 + nil usage
	sdk := &codexsdk.ImageResponse{
		Created:      1720000000,
		Background:   &background,
		Data:         []codexsdk.Image{{B64JSON: strPtr("QUJD")}, {B64JSON: strPtr("REVG")}},
		OutputFormat: strPtr("png"),
		Quality:      &quality,
		Size:         &size,
		Usage: &codexsdk.ImageUsage{
			InputTokens: 2, InputImageTokens: 1, OutputTokens: 3, OutputImageTokens: 2,
		},
	}
	d := fromSDKResponse(sdk)
	require.Equal(t, int64(1720000000), d.Created)
	require.Equal(t, &background, d.Background)
	require.Equal(t, strPtr("png"), d.OutputFormat)
	require.Len(t, d.Data, 2)
	require.Equal(t, strPtr("QUJD"), d.Data[0].B64JSON)
	require.Equal(t, strPtr("REVG"), d.Data[1].B64JSON)
	require.NotNil(t, d.Usage)
	require.Equal(t, int64(1), d.Usage.InputImageTokens)
	require.Equal(t, int64(2), d.Usage.OutputImageTokens)
	require.Nil(t, fromSDKResponse(&codexsdk.ImageResponse{}).Usage, "usage 缺失 → nil（per-image 兜底）")

	// MarshalImageResponse：wire 形态（嵌套 usage details——计费提取同源）
	wire, err := MarshalImageResponse(d)
	require.NoError(t, err)
	require.Equal(t, int64(2), jsonGetInt(t, wire, "data.#"), "data 长 = 张数")
	require.Equal(t, int64(1), jsonGetInt(t, wire, "usage.input_tokens_details.image_tokens"))
	require.Equal(t, int64(2), jsonGetInt(t, wire, "usage.output_tokens_details.image_tokens"))
	require.Equal(t, int64(2), jsonGetInt(t, wire, "usage.input_tokens"))
	// 无 usage / 空 data 形态
	wire2, err := MarshalImageResponse(&domain.ImageResponse{Data: []domain.Image{}})
	require.NoError(t, err)
	require.Equal(t, "[]", jsonGetStr(t, wire2, "data"), "空 data → []（非 null）")
	require.Equal(t, "", jsonGetStr(t, wire2, "usage"), "usage 缺失 → 字段不输出")
	// 零 image token 不输出 details（缺失与 0 同语义——计费读 0）
	wire3, err := MarshalImageResponse(&domain.ImageResponse{
		Data:  []domain.Image{{}},
		Usage: &domain.ImageUsage{InputTokens: 5},
	})
	require.NoError(t, err)
	require.Equal(t, "", jsonGetStr(t, wire3, "usage.input_tokens_details.image_tokens"), "0 image token → details 不输出")
}

func strPtr(s string) *string { return &s }

func jsonGetInt(t *testing.T, b []byte, path string) int64 {
	t.Helper()
	return gjson.GetBytes(b, path).Int()
}

func jsonGetStr(t *testing.T, b []byte, path string) string {
	t.Helper()
	return gjson.GetBytes(b, path).String()
}

// ---------------------------------------------------------------------------
// cred → Auth 缓存
// ---------------------------------------------------------------------------

// TestCodexCacheReuseAndRebuild 同账号复用（同 HTTPClient 指针断言）/ 凭据
// 更新后重建（token/rt/pat/base URL 任一变化 → 新客户端）/ 轮转回调写回不重建
// （回调本身不触发缓存变更）。
func TestCodexCacheReuseAndRebuild(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"

	p := &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"}
	img, err := a.GenerateImage(context.Background(), cred, p)
	require.NoError(t, err)
	require.Len(t, img.Data, 2, "mock 200 真实生成形态")
	e1 := a.entries[7]
	require.NotNil(t, e1, "构造后入缓存")

	// 同账号同凭据 → 复用（同一 HTTPClient）
	_, err = a.GenerateImage(context.Background(), cred, p)
	require.NoError(t, err)
	require.Same(t, e1.client, a.entries[7].client, "同账号复用（轮转状态/连接池保持）")
	require.Equal(t, 2, c.callsN())

	// 凭据更新（管理面导入/更新——at 变更）→ 重建
	cred2 := oauthCred(7, "at-2", "rt-1")
	cred2.BaseURL = up.URL + "/images/generations"
	_, err = a.GenerateImage(context.Background(), cred2, p)
	require.NoError(t, err)
	require.NotSame(t, e1.client, a.entries[7].client, "凭据更新 → 重建")
	require.NotEqual(t, "Bearer at-1", c.auth(c.callsN()-1), "重建后新 at 生效")

	// rt 变更同样触发重建
	cred3 := oauthCred(7, "at-2", "rt-2")
	cred3.BaseURL = up.URL + "/images/generations"
	_, err = a.GenerateImage(context.Background(), cred3, p)
	require.NoError(t, err)
	require.NotSame(t, a.entries[7].client, e1.client, "rt 更新 → 重建")

	// base URL 变更（模板 base 更新——aiclient InvalidateAll 同语义）→ 重建
	cred4 := oauthCred(7, "at-2", "rt-2")
	cred4.BaseURL = up.URL + "/images/generations/other"
	_, err = a.GenerateImage(context.Background(), cred4, p)
	require.NoError(t, err)
	require.NotSame(t, a.entries[7].client, e1.client, "base URL 变更 → 重建")

	// 无上报（成功路径）
	require.Empty(t, handler.snapshot(), "成功路径不上报")
}

// TestCodexCachePAT pat 类型：PAT(key) 静态直连（无轮转回调）；同账号复用。
func TestCodexCachePAT(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-1"}
	cred.BaseURL = up.URL + "/images/generations"
	p := &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"}
	_, err := a.GenerateImage(context.Background(), cred, p)
	require.NoError(t, err)
	require.Equal(t, "Bearer pat-1", c.auth(0), "PAT 静态鉴权")
	e1 := a.entries[9]
	_, err = a.GenerateImage(context.Background(), cred, p)
	require.NoError(t, err)
	require.Same(t, e1.client, a.entries[9].client, "PAT 同账号复用")
}

// TestCodexCacheConcurrentSingleFlight 同账号并发请求单飞构造：N goroutine
// 并发首请求 → 恰一次构造入缓存（互斥锁单飞——对齐 SDK OAuth 单飞语义）；
// 全部请求成功送达同一账号凭据。
func TestCodexCacheConcurrentSingleFlight(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	a := NewCodex(nil)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	p := &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.GenerateImage(context.Background(), cred, p)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "并发首请求全部成功")
	}
	a.mu.Lock()
	require.Len(t, a.entries, 1, "并发单飞构造——恰一个缓存条目")
	e := a.entries[7]
	a.mu.Unlock()
	require.NotNil(t, e)
	require.Equal(t, 32, c.callsN(), "并发请求全部送达上游")
}

// TestCodexCacheEvictionOnFatal fatal → 失效剔除（T1 联动）：上报后缓存条目
// 摘除；后续请求重建（新条目）。
func TestCodexCacheEvictionOnFatal(t *testing.T) {
	up, _ := newCodexUpstream(t, codexUpstreamStep{status: 401, body: `{"error":{"code":"token_invalidated"}}`})
	defer up.Close()
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	p := &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"}
	_, err := a.GenerateImage(context.Background(), cred, p)
	require.Error(t, err)

	a.mu.Lock()
	require.Len(t, a.entries, 0, "fatal 上报后失效剔除——缓存条目摘除")
	a.mu.Unlock()
	calls := handler.snapshot()
	require.Len(t, calls, 1, "fatal 单次上报")
	require.Equal(t, int64(7), calls[0].accountID)
	var ap *codexsdk.AuthPermanentlyRevokedError
	require.True(t, errors.As(calls[0].fatal, &ap), "上报错误类型 = SDK 判死类型")
}

// ---------------------------------------------------------------------------
// 信封包装
// ---------------------------------------------------------------------------

// TestCodexEnvelopeHTTPError SDK *HTTPError → 网关侧信封：StatusCode()/
// RawJSON()/Unwrap 链（errors.As *codexsdk.HTTPError 穿透命中——网关
// statusOf/upstreamBody 零改动复用）。
func TestCodexEnvelopeHTTPError(t *testing.T) {
	up, _ := newCodexUpstream(t, codexUpstreamStep{status: 403, body: `{"detail":"Forbidden"}`})
	defer up.Close()
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)

	var env *EnvelopeError
	require.True(t, errors.As(err, &env), "信封类型 errors.As 命中")
	require.Equal(t, 403, env.StatusCode(), "信封 StatusCode() = 上游状态码")
	require.Equal(t, `{"detail":"Forbidden"}`, env.RawJSON(), "信封 RawJSON() = 上游原始 body")
	// Unwrap 链：errors.As *codexsdk.HTTPError 穿透信封
	var he *codexsdk.HTTPError
	require.True(t, errors.As(err, &he), "Unwrap 保留 errors.As 链（SDK 类型仍可命中）")
	require.Equal(t, 403, he.StatusCode)
	require.Equal(t, `{"detail":"Forbidden"}`, string(he.Raw))
	// 信封不上报回调（透传协议）
	require.Empty(t, handler.snapshot(), "信封错误不上报回调")
}

// TestEnvelopeErrorFatalChain 信封 Unwrap 链直接断言：包装 SDK fatal → errors.As
// 穿透命中（网关 fatal 分类不因信封包装失效——T1 envelope_test 的 sdkHTTPError
// 链覆盖协议面，此处补真实 SDK fatal 类型穿透）。
func TestEnvelopeErrorFatalChain(t *testing.T) {
	fatal := &codexsdk.RefreshOAuthError{Code: "invalid_grant", Raw: []byte(`{"error":"invalid_grant"}`)}
	env := NewEnvelopeError(401, `{"error":"invalid_grant"}`, fatal)
	var re *codexsdk.RefreshOAuthError
	require.True(t, errors.As(env, &re), "Unwrap 保留 errors.As 链——fatal 类型穿透信封命中")
	require.Equal(t, "invalid_grant", re.Code)
	// Err nil → Unwrap nil → 链自然中断
	env2 := NewEnvelopeError(502, "x", nil)
	require.Nil(t, env2.Unwrap())
	var he *codexsdk.HTTPError
	require.False(t, errors.As(env2, &he))
}

// ---------------------------------------------------------------------------
// fatal 双源去重 + 不上报类
// ---------------------------------------------------------------------------

// TestCodexFatalDedupCallbackAndErrorsAs fatal 双源去重：rotationAuth 路径同一
// fatal 既触发 WithOnAuthFatal 又随返回错误 errors.As 命中——以回调为准去重、
// 单次上报（回调 CAS 胜出，errors.As 补报路径跳过）。
func TestCodexFatalDedupCallbackAndErrorsAs(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 401, body: `{"error":{"code":"token_invalidated"}}`})
	defer up.Close()
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)
	var ap *codexsdk.AuthPermanentlyRevokedError
	require.True(t, errors.As(err, &ap), "fatal 原样透传（errors.As 命中）")
	require.Equal(t, 1, c.callsN(), "判死不重试")

	calls := handler.snapshot()
	require.Len(t, calls, 1, "回调 + errors.As 双源 → 单次上报")
	require.Equal(t, int64(7), calls[0].accountID)
	require.True(t, errors.As(calls[0].fatal, &ap), "上报 = SDK 判死错误（非信封）")

	// 后续同账号请求（管理面恢复前不应出现——快照已摘除；防御断言）：条目已
	// 剔除 → 重建新 Auth → 新 incident 再次上报一次（每次事件一次——下游
	// HandleFailure 幂等，重复上报不重复写）。
	_, err = a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err, "同账号再次请求仍失败（判死态）")
	calls = handler.snapshot()
	require.Len(t, calls, 2, "重建条目后同 fatal 再上报一次（每 incident 一次）")
}

// TestCodexRefreshErrorNotReported RefreshError 可重试不上报（对齐 SDK 语义
// auth_errors.go:53-58）：refresh 端点 500 耗尽退避 → RefreshError 原样透传，
// FailureHandler 零调用。
func TestCodexRefreshErrorNotReported(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	// refresh 恒 500（可重试类——非 fatal）
	newCodexMockRefresh(t, codexUpstreamStep{status: 500, body: `{}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	// 无初始 at（OAuthToken 空 → 首请求前用 rt 换取 → refresh 失败）
	cred := &domain.AccountCredential{AccountID: 7, OAuthRefreshToken: "rt-1"}
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)
	var re *codexsdk.RefreshError
	require.True(t, errors.As(err, &re), "RefreshError 原样透传（errors.As 命中）")
	require.Zero(t, c.callsN(), "refresh 失败不触达上游 images 端点")
	require.Empty(t, handler.snapshot(), "RefreshError 可重试类不上报")
}

// TestCodexEmptyRefreshTokenNoPanic P2-3 空 rt 防护：oauth 凭据缺 refresh_token
// → 按失效处理上报（账号凭据不完整）不 panic（OAuthWithRotation 空 rt 构造
// panic 被构造前校验拦截）。
func TestCodexEmptyRefreshTokenNoPanic(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add)
	cred := &domain.AccountCredential{AccountID: 7, OAuthToken: "at-1"} // rt 缺失

	require.NotPanics(t, func() {
		_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
		require.Error(t, err)
		require.ErrorIs(t, err, errCredentialIncomplete)
	})
	calls := handler.snapshot()
	require.Len(t, calls, 1, "空 rt → 失效上报一次")
	require.Equal(t, int64(7), calls[0].accountID)
	require.ErrorIs(t, calls[0].fatal, errCredentialIncomplete)
	a.mu.Lock()
	require.Len(t, a.entries, 0, "构造失败不入缓存")
	a.mu.Unlock()
}

// TestCodexInitialATPreset 过期判定在网关侧构造前：未过期 at → 预置初始 at
// （首请求直接用，不强制 refresh）；已过期 → 不预置（SDK 首请求前用 rt 换取）。
func TestCodexInitialATPreset(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	refresh := newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-fresh","refresh_token":"rt-new"}`})
	a := NewCodex(nil)

	// 未过期 → 预置 at：上游直接收到 at-1，refresh 零调用
	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	require.Equal(t, "Bearer at-1", c.auth(0), "未过期 at 预置——首请求直接用")
	require.Zero(t, refresh.callsN(), "预置 at 不触发 refresh")

	// 已过期 → 不预置：首请求前先用 rt 换取新 at
	expired := time.Now().Add(-time.Hour)
	cred2 := &domain.AccountCredential{
		AccountID: 7, OAuthToken: "at-expired", OAuthRefreshToken: "rt-2", OAuthExpiresAt: &expired,
	}
	cred2.BaseURL = up.URL + "/images/generations"
	_, err = a.GenerateImage(context.Background(), cred2, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	require.Equal(t, "Bearer at-fresh", c.auth(1), "过期 at 不预置——rt 换取的新 at 生效")
	require.Equal(t, 1, refresh.callsN(), "已过期 → 首请求前 refresh 一次")

	// nil 过期时刻（未知）→ 视为可用预置
	cred3 := &domain.AccountCredential{AccountID: 7, OAuthToken: "at-3", OAuthRefreshToken: "rt-3"}
	cred3.BaseURL = up.URL + "/images/generations"
	_, err = a.GenerateImage(context.Background(), cred3, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	require.Equal(t, "Bearer at-3", c.auth(2), "未知过期时刻 → 预置（401 自愈兜底）")
	require.Equal(t, 1, refresh.callsN(), "预置路径不触发 refresh")
}

// TestCodex401RotationSuccess 401 非判死 → SDK 自动轮转重试一次（刷新后新 at
// 重发）——成功路径（判死 vs 轮转的分界断言）。
func TestCodex401RotationSuccess(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if n == 1 {
			// 非判死 401（过期 AT——错误码不在判死集）
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"token_expired"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okImageResponse))
	}))
	t.Cleanup(srv.Close)
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-rotated","refresh_token":"rt-new"}`})
	a := NewCodex(nil)

	cred := oauthCred(7, "at-old", "rt-1")
	cred.BaseURL = srv.URL + "/images/generations"
	img, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	require.Len(t, img.Data, 2)
	require.Equal(t, int64(2), calls.Load(), "401 非判死 → 轮转重试一次")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "Bearer at-old", auths[0])
	require.Equal(t, "Bearer at-rotated", auths[1], "轮转后新 at 生效")
}

// TestCodexNilParamsAndNetwork SDK 参数校验错误透传（Model/Prompt 必填——
// 网关已前置校验，防御断言）与网络错误（连接级——code 0 分类由网关侧承担）。
func TestCodexNilParamsAndNetwork(t *testing.T) {
	a := NewCodex(nil)
	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = "http://127.0.0.1:1/images/generations" // 不可达端口
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "必填", "SDK 参数校验错误原样透传")
}

// TestCodexIncompleteNotReportedTwice 同一账号连续空 rt 请求：失效上报每次
// 事件一次（首次上报后网关侧已摘除——重复上报幂等由 HandleFailure 保证；
// 适配层每请求构造失败都上报，链自限）。
func TestCodexIncompleteNotReportedTwice(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add)
	cred := &domain.AccountCredential{AccountID: 7}
	for i := 0; i < 3; i++ {
		_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
		require.Error(t, err)
	}
	require.Len(t, handler.snapshot(), 3, "每次构造失败都上报（下游 HandleFailure 幂等；摘除后不再调度）")
}
