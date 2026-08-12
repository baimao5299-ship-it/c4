// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
)

// HandleImagesGenerations 转发 POST /v1/images/generations（openai-images 格式；
// 全部逻辑在 handleFormat + imagesCaller——端点入口委托，同 HandleChat 形态）。
func (p *Proxy) HandleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	p.handleFormat(domain.FormatOpenAIImages, w, r)
}

// HandleImagesEdits 转发 POST /v1/images/edits（openai-images 格式；上游子路径
// 由 handleFormat 内 imagesCallerFor 按请求路径选择）。
func (p *Proxy) HandleImagesEdits(w http.ResponseWriter, r *http.Request) {
	p.handleFormat(domain.FormatOpenAIImages, w, r)
}
