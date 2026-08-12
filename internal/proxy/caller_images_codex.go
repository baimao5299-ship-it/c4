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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// errCodexImagesNotIntegrated 501：codex 适配层未装配（SetCodex 未调用——main
// 装配缺失的显式拒绝，不让凭据缺失路径误报 502/network）。
var errCodexImagesNotIntegrated = &formatError{status: http.StatusNotImplemented, msg: "codex image generation unavailable (adapter not wired)"}

// codexImagesCaller 是 codex-oauth/codex-pat 类型的 images 端点调用器（T2 §2，
// B 的 501 分流骨架落位）：网关解析请求体 → domain.ImageGenParams → 适配层
// GenerateImage（SDK 直连 codex images 端点，非流式）→ 响应统一走
// domain.ImageResponse 口径 → wire 序列化转发 + 计费提取（复用 C 的
// image_usage 提取纯函数——data 长 = 张数 + usage image_tokens → ImageCost，
// 与 api_key 直连同口径）。流式（T3）：GenerateImageStream 合成事件流 →
// streamImageGeneration（SSE 透传/keepalive/流终+abort 计费——T3 生产接线
// 点，同签名直赋适配层方法）。
// codexImagesCaller 无路径字段（评审 P3-1）：上游端点由 SDK 按参数派生
// （ImageGenParams.Images 非空 → edits，否则 generations）——与
// imagesCaller.path（直连面拼 URL）不同，codex 面端点选择归 SDK。
type codexImagesCaller struct {
	p *Proxy
}

func (c *codexImagesCaller) Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (int, []byte, bool, error) {
	p := c.p
	// 客户端请求模型（日志口径）：JSON 顶层提取 / multipart form 提取（与
	// imagesCaller 的 reqModel 提取同构；body 已在内存，零 IO）。
	reqModel := gjson.GetBytes(body, "model").String()
	contentType := r.Header.Get("Content-Type")
	if isMultipartForm(contentType) {
		reqModel = imagesMultipartModel(body, contentType)
	}
	if p.codex == nil {
		// 适配层未装配（SetCodex 未调用）：显式 501（防 nil 误走凭据缺失 502）。
		p.sched.Release(sel.AccountID)
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusNotImplemented, domain.ErrBilling, 0, usageTuple{}, start, errCodexImagesNotIntegrated.msg)
		writeErr(w, errCodexImagesNotIntegrated)
		return 0, nil, true, nil
	}
	params, err := imageParamsFromBody(body, contentType)
	if err != nil {
		// 本地参数拒绝（post-Select——Release + recordRejected + 400；评审
		// P2-1 语义：拒绝走 err_logs 审计）。模型映射改写对 codex 无字节透传
		// 约束，模型兜底走下方 sel.Model。
		p.sched.Release(sel.AccountID)
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusBadRequest, domain.ErrBilling, 0, usageTuple{}, start, err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return 0, nil, true, nil
	}
	// 模型映射：sel.Model（调度器已应用 ModelMapping——与直连路径 setModel
	// 改写同语义；multipart 直连不做改写是字节透传约束，codex 网关重建 body
	// 无此约束）。缺模型（multipart 无 form model 等边角）→ 请求模型兜底。
	if params.Model == "" {
		params.Model = sel.Model
	}
	if params.Model == "" {
		params.Model = reqModel
	}
	// cred 派生（T1 已定义）：AccountExt → AccountCredential；BaseURL = 模板
	// base 派生完整 generations 端点（空 → SDK 内置 DefaultImagesURL）。
	cred2 := domain.CredentialFromExt(sel.Ext)
	baseURL, err := codexImagesBaseURL(sel.BaseURL)
	if err != nil {
		p.sched.Release(sel.AccountID)
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusBadRequest, domain.ErrBilling, 0, usageTuple{}, start, err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return 0, nil, true, nil
	}
	cred2.BaseURL = baseURL
	if stream {
		// 流式（T3 生产接线——同签名直赋适配层 GenerateImageStream）：参数/
		// 凭据派生与上共用，事件流 → streamImageGeneration（SSE 透传 + 首事件
		// 头 + keepalive ": ping" + completed 帧 + 流终/abort 计费，全在其内）。
		return p.streamImageGeneration(ctx, w, r, reqID, groupID, start, sel, reqModel, &cred2, params, p.codex.GenerateImageStream)
	}
	img, err := p.codex.GenerateImage(ctx, &cred2, params)
	if err != nil {
		// 错误分类（骨架 statusOf/upstreamBody 零改动复用——信封协议）：
		//   - 信封（*HTTPError 包装——403 账号无生图权限等）→ 4xx 透传 /
		//     429/5xx failover 既有分类
		//   - fatal（errors.As 五类）→ 适配层已统一回调上报（账号失效标记 +
		//     FailAccount 快照摘除——failover 不重试同账号）；code 0 → 连接级
		//     MarkResult(ResultError) + 转移其它账号
		//   - RefreshError/网络 → code 0 → failover 可重试
		return statusOf(err), upstreamBody(err), false, err
	}
	// 成功：wire 序列化（客户端转发与计费提取共用同一字节——C 提取纯函数
	// ImageUsageFromResponse 与 API-key 直连同口径）。
	wire, err := sdkbridge.MarshalImageResponse(img)
	if err != nil {
		return 0, nil, false, err // 序列化失败（理论上不可达）→ 连接级 failover
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(wire)
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
	// 计费提取：data 长 = 张数 + usage image_tokens → usageTuple → finish 的
	// applyImageBilling（GetImagePrice → ImageCost，倍率整单施加）。
	ii, io, count := billing.ImageUsageFromResponse(wire)
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIImages, http.StatusOK, domain.ErrNone, usageTuple{ii: ii, io: io, tt: ii + io, img: count}, start)))
	return http.StatusOK, nil, true, nil
}

// codexImagesFor 按端点路径选 codex images 调用器（与 imagesCallerFor 同形态；
// New 构造的调用器复用，per-request 零分配）。
func (p *Proxy) codexImagesFor(r *http.Request) UpstreamCaller {
	if strings.HasSuffix(r.URL.Path, "/edits") {
		return p.codexImagesEdits
	}
	return p.codexImagesGenerations
}

// codexImagesBaseURL 模板 base → SDK WithBaseURL 覆盖值（**完整 generations
// 端点直用**——SDK 语义 client.go:137-144：覆盖值按完整端点直用，edits 由尾段
// generations→edits 派生，覆盖值必须传 generations 端点）。模板 base 空 → ""
// （SDK 内置 DefaultImagesURL）；解析失败 → 错误（配置错误本地拒绝，与直连
// 路径 parseFullURL 错误同语义）。
func codexImagesBaseURL(base string) (string, error) {
	if base == "" {
		return "", nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid template base URL %q: %w", base, err)
	}
	return u.JoinPath("images/generations").String(), nil
}

// imageParamsFromBody 请求体 → domain.ImageGenParams（T2 §2：网关解析传结构体
// ——SDK 不做 HTTP 协议解析）。JSON：顶层提取（model/prompt 必填；n/size/
// quality/background 可选；edits 输入 images:[{image_url}]）；multipart：form
// 字段（model/prompt/n/size/quality/background）+ 图片文件 part（FormName
// image 前缀 → Raw 字节，SDK 内部转 data URL）。
func imageParamsFromBody(body []byte, contentType string) (*domain.ImageGenParams, error) {
	if isMultipartForm(contentType) {
		return imageParamsMultipart(body, contentType)
	}
	return imageParamsJSON(body)
}

func imageParamsJSON(body []byte) (*domain.ImageGenParams, error) {
	if !json.Valid(body) { // 防御：handleFormat 已过 json.Valid 硬门
		return nil, errors.New("invalid request body: invalid JSON")
	}
	model := gjson.GetBytes(body, "model").String()
	prompt := gjson.GetBytes(body, "prompt").String()
	if model == "" {
		return nil, errors.New("invalid request body: model required")
	}
	if prompt == "" {
		return nil, errors.New("invalid request body: prompt required")
	}
	p := &domain.ImageGenParams{Model: model, Prompt: prompt}
	if nv := gjson.GetBytes(body, "n"); nv.Type == gjson.Number {
		n := int(nv.Int())
		p.N = &n
	}
	if sv := gjson.GetBytes(body, "size"); sv.Type == gjson.String {
		s := sv.String()
		p.Size = &s
	}
	if qv := gjson.GetBytes(body, "quality"); qv.Type == gjson.String {
		s := qv.String()
		p.Quality = &s
	}
	if bv := gjson.GetBytes(body, "background"); bv.Type == gjson.String {
		s := bv.String()
		p.Background = &s
	}
	// edits 输入图（JSON 形态 images:[{image_url}]，官方文档实证）；file_id
	// 形态不映射（需文件上传面，SDK 无此能力——忽略）。
	for _, ir := range gjson.GetBytes(body, "images").Array() {
		u := ir.Get("image_url").String()
		if u == "" {
			continue
		}
		uu := u
		p.Images = append(p.Images, domain.ImageRef{ImageURL: &uu})
	}
	return p, nil
}

func imageParamsMultipart(body []byte, contentType string) (*domain.ImageGenParams, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, errors.New("invalid request body: multipart content type parse failed")
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	p := &domain.ImageGenParams{}
	var images []domain.ImageRef
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid request body: multipart parse failed")
		}
		switch part.FormName() {
		case "model":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			p.Model = strings.TrimSpace(string(b))
		case "prompt":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			p.Prompt = strings.TrimSpace(string(b))
		case "n":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				p.N = &v
			}
		case "size":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			s := strings.TrimSpace(string(b))
			if s != "" {
				p.Size = &s
			}
		case "quality":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			s := strings.TrimSpace(string(b))
			if s != "" {
				p.Quality = &s
			}
		case "background":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			s := strings.TrimSpace(string(b))
			if s != "" {
				p.Background = &s
			}
		default:
			// 图片文件 part（image / image[] 等 FormName）→ Raw 字节（body 已
			// 在内存 MaxBytesReader 限界，SDK 内部转 data URL）；其余字段忽略。
			if strings.HasPrefix(part.FormName(), "image") {
				b, err := io.ReadAll(part)
				if err != nil {
					return nil, errors.New("invalid request body: image part read failed")
				}
				images = append(images, domain.ImageRef{Raw: b})
			}
		}
	}
	if p.Model == "" {
		return nil, errors.New("invalid request body: model required")
	}
	if p.Prompt == "" {
		return nil, errors.New("invalid request body: prompt required")
	}
	p.Images = images
	return p, nil
}
