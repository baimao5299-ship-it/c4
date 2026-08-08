//go:build e2e && windows

package e2e

import "golang.org/x/sys/windows"

// windowsGenerateCtrlBreak 向指定进程组投递 CTRL_BREAK_EVENT（Windows 优雅
// 停机：Go 的 os.Process.Signal 仅支持 Kill，需控制台事件——子进程以
// CREATE_NEW_PROCESS_GROUP 启动，组 id = 子进程 pid；main 对 os.Interrupt
// 与 SIGTERM 走同一 NotifyContext 优雅路径）。
func windowsGenerateCtrlBreak(processGroupID uint32) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, processGroupID)
}
