// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e && !windows

package e2e

// windowsGenerateCtrlBreak 非 Windows 平台无实现（SIGTERM 路径已覆盖）。
func windowsGenerateCtrlBreak(processGroupID uint32) error { return nil }
