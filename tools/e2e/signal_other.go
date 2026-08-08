//go:build e2e && !windows

package e2e

// windowsGenerateCtrlBreak 非 Windows 平台无实现（SIGTERM 路径已覆盖）。
func windowsGenerateCtrlBreak(processGroupID uint32) error { return nil }
