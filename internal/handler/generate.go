// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// 管理面（/admin）：排除 user tag；生成到本包。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -exclude-tags user -package handler -o api.gen.go ../../openapi/openapi.yaml
// 用户面（/user）：仅 user tag；独立包（共享 schema 类型在各自包内不冲突）。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags user -package user -o user/api.gen.go ../../openapi/openapi.yaml
