// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// Codex 是 codex SDK 适配层（T2 §1——SDK 调用集中于此，codexsdk import 仅限
// 本文件族）：cred → Auth 账号级缓存 + GenerateImage 包装 + 信封包装 +
// fatal → 统一回调（双源去重）。新增能力（T3 流式 / T4 Dial / T6 resp）同形态
// 扩展本文件。
type Codex struct {
	mu      sync.Mutex
	entries map[int64]*codexEntry // accountID → 客户端缓存（同账号复用）
	failure FailureHandler        // T1 统一失效回调；nil = no-op（测试/未装配）
}

// codexEntry 单账号缓存条目：Auth/HTTPClient + 重建判定签名 + fatal 已上报标记
// （双源去重——回调路径与 errors.As 路径共享同一 CAS）。
type codexEntry struct {
	accountID int64
	client    *codexsdk.HTTPClient
	sig       string // 凭据签名（外部凭据变更 → 重建）
	reported  atomic.Bool
}

// NewCodex 构造 codex 适配层。failure 为 T1 统一失效回调（适配层构造注册
// WithOnAuthFatal → 回调；nil = 上报 no-op——测试替身形态）。
func NewCodex(failure FailureHandler) *Codex {
	return &Codex{failure: failure, entries: make(map[int64]*codexEntry)}
}

// GenerateImage 非流式生图包装（T2 §1）：cred → 缓存取 HTTPClient →
// c.GenerateImage(ctx, toSDKParams(p))；domain↔codexsdk 双向转换集中本文件。
// 错误翻译（translateError）：SDK *HTTPError → 网关侧信封错误（EnvelopeError——
// StatusCode()/RawJSON()/Unwrap 链，网关 statusOf/upstreamErrMsg 零改动复用）；
// fatal 五类（errors.As）→ 统一回调单次上报（双源去重）+ 原样透传（SDK 已
// 不包装，errors.As 可命中）；RefreshError 可重试不上报。cred.BaseURL = 模板
// base 派生完整 generations 端点（空 → SDK 内置 DefaultImagesURL）。
func (a *Codex) GenerateImage(ctx context.Context, cred *domain.AccountCredential, p *domain.ImageGenParams) (*domain.ImageResponse, error) {
	e, err := a.clientFor(cred)
	if err != nil {
		return nil, err
	}
	resp, err := e.client.GenerateImage(ctx, toSDKParams(p))
	if err != nil {
		return nil, a.translateError(e, err)
	}
	return fromSDKResponse(resp), nil
}

// GenerateImageStream 流式生图包装（T3 生产接线——合成流式：SDK 内部非流式
// 调 + 等待期 keepalive 合成 + completed 逐张合成，网关零合成逻辑）：cred →
// 缓存取 HTTPClient → c.GenerateImageStream(ctx, toSDKParams(p), fn)；事件翻
// 译 codexsdk.ImageStreamEvent → domain.ImageStreamEvent（Type/B64JSON/Usage
// 逐字段映射——usage 平铺直透，网关 completed 帧 JSON tag 直透同一口径）；
// 错误翻译同 GenerateImage（translateError——信封/fatal 统一回调/refresh 分
// 类复用；fn 回调错误经 translateError 原样透传——非 SDK 错误不过滤）。
func (a *Codex) GenerateImageStream(ctx context.Context, cred *domain.AccountCredential, p *domain.ImageGenParams, fn func(domain.ImageStreamEvent) error) error {
	e, err := a.clientFor(cred)
	if err != nil {
		return err
	}
	return e.client.GenerateImageStream(ctx, toSDKParams(p), func(ev codexsdk.ImageStreamEvent) error {
		var usage *domain.ImageUsage
		if ev.Usage != nil {
			usage = &domain.ImageUsage{
				InputTokens:       ev.Usage.InputTokens,
				InputImageTokens:  ev.Usage.InputImageTokens,
				OutputTokens:      ev.Usage.OutputTokens,
				OutputImageTokens: ev.Usage.OutputImageTokens,
			}
		}
		return fn(domain.ImageStreamEvent{Type: ev.Type, B64JSON: ev.B64JSON, Usage: usage})
	})
}

// clientFor cred → Auth 账号级缓存取 HTTPClient（构造冷面——每账号首次/凭据
// 变更后；互斥锁 + 签名比对，同账号并发请求单飞构造——对齐 SDK OAuth 单飞
// refresh 语义）：
//   - 同账号复用（轮转状态/连接池保持；sig 相同直接返回）
//   - 仅外部凭据变更（管理面导入/更新——token/rt/pat/base URL 任一变化 → sig
//     不同）后重建；**轮转回调写回不重建**（回调写回的是本 Auth 内部已更新
//     的状态，重建丢 at 缓存破坏轮转连续性——写回走 T5 管理面通道，不经缓存）
//   - 失效剔除（T1 联动）：fatal 上报后 evict，恢复后重建
//   - 空 rt 防护（P2-3）：codex-oauth 缺 refresh_token → 按失效上报（账号凭据
//     不完整）不 panic（OAuthWithRotation 空 rt 构造 panic）；PAT 走 PAT(key)
//     无此面
func (a *Codex) clientFor(cred *domain.AccountCredential) (*codexEntry, error) {
	sig := credSig(cred)
	a.mu.Lock()
	if e := a.entries[cred.AccountID]; e != nil && e.sig == sig {
		a.mu.Unlock()
		return e, nil
	}
	e := &codexEntry{accountID: cred.AccountID, sig: sig}
	auth, err := a.buildAuth(cred, e)
	if err == nil {
		var opts []codexsdk.Option
		if cred.BaseURL != "" {
			opts = append(opts, codexsdk.WithBaseURL(cred.BaseURL))
		}
		e.client = codexsdk.NewHTTPClient(auth, opts...)
		a.entries[cred.AccountID] = e
	}
	a.mu.Unlock()
	if err != nil {
		// 构造失败（空 rt 等——P2-3）：锁外上报（reportFatal → evict 需取
		// a.mu——锁内调用即重入死锁；sync.Mutex 不可重入）。
		a.reportFatal(e, err)
		return nil, err
	}
	return e, nil
}

// buildAuth 按 cred 构造 SDK Auth（构造前校验——P2-3）：
//   - codex-oauth：OAuthWithRotation(rt, WithOnAuthFatal(统一回调) [,
//     WithInitialAccessToken(at)])——过期判定在网关侧构造前：OAuthExpiresAt 已
//     过期 → 不传 WithInitialAccessToken（SDK 走初始 at 缺省路径，首请求前用
//     rt 换取——auth_oauth.go:106-109 只判非空不判过期，401 自愈）；未过期/
//     未知（nil）→ 预置单参 at 避免首调用强制 refresh
//   - codex-pat：PAT(key)
//   - 空 rt（oauth 缺 refresh_token）→ 上报失效（凭据不完整）并返回错误，不
//     panic
func (a *Codex) buildAuth(cred *domain.AccountCredential, e *codexEntry) (codexsdk.Auth, error) {
	if cred.PATKey != "" {
		return codexsdk.PAT(cred.PATKey), nil
	}
	if cred.OAuthRefreshToken == "" {
		return nil, errCredentialIncomplete // 上报在 clientFor 锁外执行（见 clientFor）
	}
	opts := []codexsdk.OAuthOption{
		// 统一回调装配（T2 §3）：SDK 判死（RT 判死码 / token 端点 401 / 账号
		// 禁用 / AT 401 判死 / 回调连续失败）→ 双源去重单次上报
		codexsdk.WithOnAuthFatal(func(fatal error) { a.reportFatal(e, fatal) }),
	}
	if atUsable(cred) {
		opts = append(opts, codexsdk.WithInitialAccessToken(cred.OAuthToken))
	}
	return codexsdk.OAuthWithRotation(cred.OAuthRefreshToken, opts...), nil
}

// errCredentialIncomplete 凭据不完整（P2-3 构造前校验——oauth 类型缺
// refresh_token；按失效处理上报——账号凭据不完整，不 panic）。
var errCredentialIncomplete = errors.New("codexsdk: 凭据不完整（oauth 缺 refresh_token，账号需重新导入）")

// atUsable 初始 at 预置判定（过期判定在网关侧构造前）：at 非空且未过期（nil
// 过期时刻 = 未知 → 视为可用，401 自愈兜底）。
func atUsable(cred *domain.AccountCredential) bool {
	if cred.OAuthToken == "" {
		return false
	}
	if cred.OAuthExpiresAt == nil {
		return true
	}
	return cred.OAuthExpiresAt.After(time.Now())
}

// credSig 凭据签名（重建判定）：外部凭据变更（管理面导入/更新——token/rt/pat/
// base URL 任一变化）→ 重建。过期时刻不参与签名（构造时的初始 at 预置决策已
// 经生效；过期 at 由 SDK 401 自愈轮转，无需重建）。
//
// 分隔符用 \x00（评审 P3-3）："|" 在理论上可被 token 内容携带（碰撞误重建——
// 仅多构造一次，无害但脏）；\x00 为 Go 字符串中不可现字符（OAuth token/PAT
// base64url 字符集、base URL 经 url.Parse 拒绝控制字符——构造前校验）。
func credSig(c *domain.AccountCredential) string {
	return c.OAuthToken + "\x00" + c.OAuthRefreshToken + "\x00" + c.PATKey + "\x00" + c.BaseURL
}

// reportFatal fatal 统一上报（双源去重核心）：rotationAuth 路径同一 fatal 既
// 触发 WithOnAuthFatal 又随返回错误 errors.As 命中——**以回调为准去重、单次
// 上报**（CAS 胜者上报；败者并发调用/errors.As 补报路径跳过）。上报后失效
// 剔除（T1 联动——账号已判死，缓存条目随弃，管理面恢复后重建）。
func (a *Codex) reportFatal(e *codexEntry, fatal error) {
	if !e.reported.CompareAndSwap(false, true) {
		return
	}
	if a.failure != nil {
		a.failure(e.accountID, fatal)
	}
	a.evict(e.accountID)
}

// evict 失效剔除（缓存条目摘除；不存在 = no-op——并发/重复上报安全）。
func (a *Codex) evict(accountID int64) {
	a.mu.Lock()
	delete(a.entries, accountID)
	a.mu.Unlock()
}

// translateError SDK 错误 → 网关侧错误翻译（错误契约）：
//   - fatal 五类（errors.As——RefreshOAuthError / AuthPermanentlyRevokedError /
//     AccountDisabledError / CallbackDeliveryError）→ 双源去重单次上报（回调
//     路径 CAS 已胜出则此处跳过；PAT/无回调路径此处补报）——原样透传不包装
//     （SDK 已保证 errors.As 可命中）
//   - RefreshError → 不上报（可重试——对齐 SDK 语义 auth_errors.go:53-58，
//     网关按既有 failover 分类处理）
//   - *HTTPError → 信封包装（EnvelopeError：StatusCode()/RawJSON()/Unwrap 链）
//   - 其余（网络/解析等）原样透传（code 0 连接级分类）
func (a *Codex) translateError(e *codexEntry, err error) error {
	if f := asFatal(err); f != nil {
		// 双源去重（评审 P3-2——与 reportFatal 同语义，直接复用）：
		// CAS 在回调路径已胜出则此处跳过（单次上报）；PAT/无回调路径此处补报
		a.reportFatal(e, f)
		return err
	}
	var he *codexsdk.HTTPError
	if errors.As(err, &he) {
		return NewEnvelopeError(he.StatusCode, string(he.Raw), he)
	}
	return err
}

// asFatal 判定 SDK 错误是否为账号级终止类（fatal 五类，errors.As 穿透信封
// 包装链——EnvelopeError.Unwrap 保留链）。RefreshError 不在 fatal 集（可重试
// ——auth_errors.go:53-58 语义）。
func asFatal(err error) error {
	var (
		re *codexsdk.RefreshOAuthError
		ap *codexsdk.AuthPermanentlyRevokedError
		ad *codexsdk.AccountDisabledError
		cd *codexsdk.CallbackDeliveryError
	)
	if errors.As(err, &re) || errors.As(err, &ap) || errors.As(err, &ad) || errors.As(err, &cd) {
		return err
	}
	return nil
}

// --- domain ↔ codexsdk 双向转换（集中本文件 + 转换单测防漂移） ---

// toSDKParams domain.ImageGenParams → codexsdk.ImageGenParams（字段同构；
// nil 指针/空切片语义保留——可选字段不发）。SDK 只读消费转换产物，无别名风险。
func toSDKParams(p *domain.ImageGenParams) *codexsdk.ImageGenParams {
	if p == nil {
		return nil
	}
	s := &codexsdk.ImageGenParams{
		Model:      p.Model,
		Prompt:     p.Prompt,
		N:          p.N,
		Size:       p.Size,
		Quality:    p.Quality,
		Background: p.Background,
	}
	if len(p.Images) > 0 {
		s.Images = make([]codexsdk.ImageRef, len(p.Images))
		for i := range p.Images {
			s.Images[i] = codexsdk.ImageRef{ImageURL: p.Images[i].ImageURL, Raw: p.Images[i].Raw}
		}
	}
	return s
}

// fromSDKResponse codexsdk.ImageResponse → domain.ImageResponse（字段同构平铺；
// usage 缺失 → nil——网关 per-image 分量兜底）。
func fromSDKResponse(r *codexsdk.ImageResponse) *domain.ImageResponse {
	out := &domain.ImageResponse{
		Created:      r.Created,
		Background:   r.Background,
		OutputFormat: r.OutputFormat,
		Quality:      r.Quality,
		Size:         r.Size,
	}
	if len(r.Data) > 0 {
		out.Data = make([]domain.Image, len(r.Data))
		for i := range r.Data {
			out.Data[i] = domain.Image{B64JSON: r.Data[i].B64JSON}
		}
	}
	if r.Usage != nil {
		out.Usage = &domain.ImageUsage{
			InputTokens:       r.Usage.InputTokens,
			InputImageTokens:  r.Usage.InputImageTokens,
			OutputTokens:      r.Usage.OutputTokens,
			OutputImageTokens: r.Usage.OutputImageTokens,
		}
	}
	return out
}

// --- domain.ImageResponse → 上游 wire 序列化（客户端转发 + 计费提取共用） ---

// imageDataWire / imageUsageWire 是上游 images 端点响应 wire 形态（嵌套 usage
// details——对齐 codex-sdk imageResponseWire / billing.ImageUsageFromResponse
// 提取路径：usage.input/output_tokens_details.image_tokens + data 数组长）。
type imageDataWire struct {
	B64JSON *string `json:"b64_json"`
}

type imageTokensWire struct {
	ImageTokens int64 `json:"image_tokens"`
}

type imageUsageWire struct {
	InputTokens   int64            `json:"input_tokens"`
	OutputTokens  int64            `json:"output_tokens"`
	InputDetails  *imageTokensWire `json:"input_tokens_details"`
	OutputDetails *imageTokensWire `json:"output_tokens_details"`
}

type imageResponseWire struct {
	Created      int64           `json:"created"`
	Background   *string         `json:"background,omitempty"`
	Data         []imageDataWire `json:"data"`
	OutputFormat *string         `json:"output_format,omitempty"`
	Quality      *string         `json:"quality,omitempty"`
	Size         *string         `json:"size,omitempty"`
	Usage        *imageUsageWire `json:"usage,omitempty"`
}

// MarshalImageResponse 把 domain.ImageResponse 序列化为上游 wire 形态
// （客户端转发与计费提取共用同一字节——billing.ImageUsageFromResponse 与
// API-key 直连同口径：data 长 = 张数 + usage image_tokens）。usage 缺失 → 不
// 输出 usage 字段（上游未提供语义——per-image 分量兜底）。
func MarshalImageResponse(r *domain.ImageResponse) ([]byte, error) {
	w := imageResponseWire{
		Created:      r.Created,
		Background:   r.Background,
		OutputFormat: r.OutputFormat,
		Quality:      r.Quality,
		Size:         r.Size,
	}
	if r.Data != nil {
		w.Data = make([]imageDataWire, len(r.Data))
		for i := range r.Data {
			w.Data[i] = imageDataWire{B64JSON: r.Data[i].B64JSON}
		}
	} else {
		w.Data = []imageDataWire{}
	}
	if r.Usage != nil {
		w.Usage = &imageUsageWire{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
		}
		if r.Usage.InputImageTokens != 0 {
			w.Usage.InputDetails = &imageTokensWire{ImageTokens: r.Usage.InputImageTokens}
		}
		if r.Usage.OutputImageTokens != 0 {
			w.Usage.OutputDetails = &imageTokensWire{ImageTokens: r.Usage.OutputImageTokens}
		}
	}
	return json.Marshal(w)
}
