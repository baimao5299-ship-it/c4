// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	enterrlog "github.com/is7qin/c3api/internal/ent/errlog"
	entusagelog "github.com/is7qin/c3api/internal/ent/usagelog"
)

// --- 假上游：images 端点（/v1/images/generations|edits） ---
// 断言请求面（路径/鉴权/Content-Type/body），返回标准 ImageResponse（非流式）
// 或 SSE（stream=true）。

type imagesUpstreamCapture struct {
	mu          sync.Mutex
	calls       int
	path        string
	auth        string
	contentType string
	body        []byte
}

func fakeImagesUpstream(t *testing.T, wantPath string) (*httptest.Server, *imagesUpstreamCapture) {
	t.Helper()
	c := &imagesUpstreamCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.calls++
		c.path = r.URL.Path
		c.auth = r.Header.Get("Authorization")
		c.contentType = r.Header.Get("Content-Type")
		c.body = b
		c.mu.Unlock()
		if r.URL.Path != wantPath {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		if gjson.GetBytes(b, "stream").Type == gjson.True {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"image_generation.completed","data":[{"b64_json":"QUJD"}]}`)
			fl.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 1720000000,
			"data": []any{
				map[string]any{"b64_json": "QUJD"},
				map[string]any{"url": "https://upstream.example/1.png"},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

// newTestImagesProxy 构造 images 格式模板测试代理（api_key 类型 + images
// 格式；bill 可注入计费钩子）。capture 为捕获落库（用量断言）。
func newTestImagesProxy(t *testing.T, upstream string, bill *BillingHooks) (*Proxy, *captureLogStore) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	}
	store := &captureLogStore{}
	return newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, bill), store
}

// fakeImagePriceLookup 内存 image 价快照（ImagePriceLookup 实现；缺失 →
// ErrNotFound 语义）。
type fakeImagePriceLookup struct {
	m map[string]*domain.ImagePrice
}

func (f *fakeImagePriceLookup) GetImagePrice(model string) (*domain.ImagePrice, error) {
	if p, ok := f.m[model]; ok {
		return p, nil
	}
	return nil, errors.New("no image price")
}

func perImagePriceRow(model string) *domain.ImagePrice {
	return &domain.ImagePrice{
		Model:                   model,
		OutputCostPerImageMilli: ptr(int64(5400)), // aiml 形态：仅 per-image 分量（无 token 价）
		Source:                  domain.PricingSourceManual,
	}
}

func ptr[T any](v T) *T { return &v }

// TestImagesGenerationsJSONDirect generations JSON 非流式直连：api_key 类型
// 模板 → 转发模板 base_url + /v1/images/generations；JSON 形态 setModel 模型
// 映射改写（客户端 img-1 → 上游 gpt-image-1）；响应原样透传。
func TestImagesGenerationsJSONDirect(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
		ModelMapping:     map[string]string{"img-1": "gpt-image-1"},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"img-1","prompt":"a cat","n":2}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"b64_json":"QUJD"`, "上游响应原样透传")
	require.Contains(t, rec.Body.String(), `"url":"https://upstream.example/1.png"`)
	c.mu.Lock()
	require.Equal(t, "/v1/images/generations", c.path)
	require.Equal(t, "Bearer sk-upstream", c.auth)
	require.Equal(t, "gpt-image-1", gjson.GetBytes(c.body, "model").String(), "JSON 形态 setModel 映射改写")
	require.Equal(t, "a cat", gjson.GetBytes(c.body, "prompt").String(), "其余字段原样")
	c.mu.Unlock()

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.FormatOpenAIImages, store.logs[0].Format, "images 请求落 usage_logs 格式 openai-images")
	require.Equal(t, "img-1", store.logs[0].Model, "日志 Model = 客户端请求模型")
	require.Equal(t, "gpt-image-1", store.logs[0].MappedModel, "日志 MappedModel = 映射后模型")
}

// TestImagesEditsJSONDirect edits JSON 非流式直连（generations 之外的第二个
// 端点：上游子路径 /v1/images/edits）。
func TestImagesEditsJSONDirect(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/edits")
	defer up.Close()
	p, _ := newTestImagesProxy(t, up.URL, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(
		`{"model":"gpt-image-1","image":"https://example.com/in.png"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesEdits(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	c.mu.Lock()
	require.Equal(t, "/v1/images/edits", c.path)
	require.Equal(t, "Bearer sk-upstream", c.auth)
	require.Equal(t, "gpt-image-1", gjson.GetBytes(c.body, "model").String())
	c.mu.Unlock()
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesMultipartHardGateSkippedAndPassthrough multipart 专用 body 分支
// （P1-2）：body 为非 JSON（含图片文件字节）→ 必须 200（json.Valid 硬门对
// multipart 跳过——不跳过则 400 误杀）；body 字节与 Content-Type（含
// boundary）原样透传上游；model 从 form 字段取（映射不回写——form model
// 原样透传）。
func TestImagesMultipartHardGateSkippedAndPassthrough(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/edits")
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
		ModelMapping:     map[string]string{"img-1": "gpt-image-1"},
	}
	p := newTestProxyTplCapture(t, tpl, 1, true)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("model", "img-1"))
	require.NoError(t, mw.WriteField("prompt", "make it red"))
	fw, err := mw.CreateFormFile("image", "photo.png")
	require.NoError(t, err)
	_, err = fw.Write([]byte("PNG-binary-junk-that-is-not-json"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	body := buf.Bytes()
	ct := mw.FormDataContentType()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer gk-1")
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	p.HandleImagesEdits(rec, req)

	require.Equal(t, 200, rec.Code, "multipart 不得撞 json.Valid 硬门：body=%s", rec.Body.String())
	c.mu.Lock()
	require.Equal(t, 1, c.calls, "multipart 请求必须转发上游")
	require.Equal(t, ct, c.contentType, "multipart Content-Type（含 boundary）原样透传")
	require.Equal(t, body, c.body, "multipart body 字节原样透传（图片文件不解析不重写）")
	// 上游侧解析 form：model 为客户端原值（img-1，映射不回写——spec §5.1 声明）
	mr := multipart.NewReader(bytes.NewReader(c.body), boundaryOf(ct))
	formModel := ""
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			b, _ := io.ReadAll(part)
			formModel = string(b)
		}
	}
	require.Equal(t, "img-1", formModel, "multipart 形态不做 setModel 改写（form model 原样透传）")
	c.mu.Unlock()
	require.NoError(t, p.rec.Close(context.Background()))
}

func boundaryOf(ct string) string {
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	return params["boundary"]
}

// TestImagesNoImagePrice402 生死判定（空行语义）：计费启用且 image_price 快照
// 无该模型行 → 402，上游一个请求都不许收到（对齐 chat 缺价预检语义，不按
// 0 计价）。
func TestImagesNoImagePrice402(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	p := newTestImagesProxyWithBill(t, up.URL, &BillingHooks{
		Prices:      &fakePriceLookup{m: map[string]*domain.Pricing{}},
		ImagePrices: &fakeImagePriceLookup{m: map[string]*domain.ImagePrice{}},
	}, &captureLogStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, http.StatusPaymentRequired, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "no price", "402 文案说明缺价")
	require.Zero(t, hits.Load(), "缺价不得转发上游")
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesPureImageModelNotKilledByChatPrecheck P1-1 核心断言：纯 image 价
// 模型（aiml 形态——仅 per-image 分量，无文本价 → 无 pricings 行）在 images
// 端点不被 chat 价预检（GetPrice）误杀——预检按格式切换：images 查
// GetImagePrice（有行 → 放行），跳过 GetPrice（空 chat 价表 → 修复前 402）。
func TestImagesPureImageModelNotKilledByChatPrecheck(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestImagesProxyWithBill(t, up.URL, &BillingHooks{
		Prices:      &fakePriceLookup{m: map[string]*domain.Pricing{}}, // chat 价表空（纯 image 价模型）
		ImagePrices: &fakeImagePriceLookup{m: map[string]*domain.ImagePrice{"gpt-image-1": perImagePriceRow("gpt-image-1")}},
	}, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "纯 image 价模型不得被 chat 价预检误杀：body=%s", rec.Body.String())
	require.Equal(t, 1, c.calls, "预检通过 → 正常转发")
	require.NoError(t, p.rec.Close(context.Background()))
	// 计费集成在 T2/C（本任务路由面）：images 日志跳过 chat 价计费（Cost 0、
	// 无 no_price 标记噪音、无价格快照列）。
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Zero(t, store.logs[0].Cost, "Task B 不接入 ImageCost（计费在 C）")
	require.Equal(t, "auto", store.logs[0].BillingTier, "service_tier 归一化照常（计费启用路径）")
	require.Nil(t, store.logs[0].PriceInputMillis, "images 日志不落 chat 价快照列（nil）")
}

// TestImagesNoImagePriceWhenImagePricesNil bill 装配但 ImagePrices 未注入（未
// 装配形态）→ 不预检（等价计费全关），请求放行。
func TestImagesNoImagePriceWhenImagePricesNil(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	p := newTestImagesProxyWithBill(t, up.URL, &BillingHooks{
		Prices:   &fakePriceLookup{m: map[string]*domain.Pricing{}},
		Balances: billingBalances(),
	}, &captureLogStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)
	require.Equal(t, 200, rec.Code, "ImagePrices 未装配不预检：body=%s", rec.Body.String())
	require.Equal(t, 1, c.calls)
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesStreamingSSE 流式直连透传：JSON stream=true → 上游 SSE 原样透传
// （首事件 + [DONE]），模型改写照常（JSON 形态）。
func TestImagesStreamingSSE(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
		ModelMapping:     map[string]string{"img-1": "gpt-image-1"},
	}
	p := newTestProxyTplCapture(t, tpl, 1, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"img-1","prompt":"cat","stream":true}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, "data: [DONE]")
	require.Contains(t, body, `"type":"image_generation.completed"`, "上游 SSE 事件原样透传")
	c.mu.Lock()
	require.Equal(t, "gpt-image-1", gjson.GetBytes(c.body, "model").String(), "流式 JSON 同样模型改写")
	c.mu.Unlock()
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodexNotIntegrated501 codex 分流骨架：codex-oauth 模板在 images
// 端点选号命中 → 501 明确"未接入"（SDK 调用 T2/T3 接；未接入前不得误报
// 502/network），上游不收请求。
func TestImagesCodexNotIntegrated501(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	}
	p := newTestProxyTplCapture(t, tpl, 1, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, http.StatusNotImplemented, rec.Code, "codex 未接入必须显式 501：body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "not integrated", "501 文案明确未接入")
	require.Zero(t, hits.Load(), "未接入不得转发上游")
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodexPATNotIntegrated501 codex-pat 同 codex-oauth（分流骨架两类型
// 一并覆盖）。
func TestImagesCodexPATNotIntegrated501(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeCodexPAT,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	}
	p := newTestProxyTplCapture(t, tpl, 1, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{"model":"gpt-image-1"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesEdits(rec, req)
	require.Equal(t, http.StatusNotImplemented, rec.Code, "body=%s", rec.Body.String())
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesResponsesSpecialDirect responses-special 类型直连（用户裁决：两
// 类型都支持两个端点）：凭据取用成功（P4 502 消灭同款——注册表必须含
// responses-special provider）+ 上游收到账号 key。
func TestImagesResponsesSpecialDirect(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeResponsesSpecial,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	}
	p := newTestProxyTplCapture(t, tpl, 1, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "responses-special 直连必须可用：body=%s", rec.Body.String())
	c.mu.Lock()
	require.Equal(t, "Bearer sk-upstream", c.auth, "responses-special 凭据 = 账号 upstream_key")
	c.mu.Unlock()
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesFormatValidatorOpenaiImages 枚举扩展（spec §4.3）：usage_logs /
// err_logs 的 format 枚举必须接受 openai-images（否则 images 请求落账 COPY
// 恒失败——评审 D4）。
func TestImagesFormatValidatorOpenaiImages(t *testing.T) {
	require.NoError(t, entusagelog.FormatValidator(entusagelog.Format(domain.FormatOpenAIImages)))
	require.NoError(t, enterrlog.FormatValidator(enterrlog.Format(domain.FormatOpenAIImages)))
	require.Equal(t, entusagelog.FormatOpenaiImages, entusagelog.Format(domain.FormatOpenAIImages))
}

// TestImagesMultipartModelField extraction 单测：model 从 form 字段取（图片
// 文件 part 跳过）；缺失 → ""；无 boundary → ""。
func TestImagesMultipartModelField(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "x")
	fw, _ := mw.CreateFormFile("image", "p.png")
	_, _ = fw.Write([]byte("file-bytes"))
	_ = mw.WriteField("model", "  gpt-image-1  ")
	_ = mw.Close()
	body := buf.Bytes()
	require.Equal(t, "gpt-image-1", imagesMultipartModel(body, mw.FormDataContentType()), "form model 字段提取（去空白）")

	// 缺失 model
	var buf2 bytes.Buffer
	mw2 := multipart.NewWriter(&buf2)
	_ = mw2.WriteField("prompt", "x")
	_ = mw2.Close()
	require.Equal(t, "", imagesMultipartModel(buf2.Bytes(), mw2.FormDataContentType()), "无 model 字段 → 空")

	// 无 boundary（非法 Content-Type）
	require.Equal(t, "", imagesMultipartModel(body, "multipart/form-data"), "boundary 缺失 → 空")
	require.False(t, isMultipartForm("application/json"), "JSON 不是 multipart")
	require.True(t, isMultipartForm(mw.FormDataContentType()), "multipart/form-data 判定")
}

// newTestImagesProxyWithBill 构造 images 模板 + 注入计费钩子的测试代理。
func newTestImagesProxyWithBill(t *testing.T, upstream string, bill *BillingHooks, store *captureLogStore) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	}
	return newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, bill)
}

func billingBalances() *billing.Balances {
	return billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil)
}
