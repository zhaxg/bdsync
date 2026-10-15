package main

import (
	"os"
	"syscall"
)

// stopSignals -- 进程停止信号集合（Ctrl+C / 关窗口 / 服务停止 / kill）。
//
// Windows 下不必也不能注册 CTRL_BREAK：Go runtime 的 ctrlHandler 已把
// CTRL_C_EVENT 与 CTRL_BREAK_EVENT 都映射成 SIGINT（syscall.SIGBREAK 在
// Windows 版 Go 里根本不存在），而 CTRL_CLOSE_EVENT / CTRL_SHUTDOWN_EVENT
// 映射成 SIGTERM ---- nssm 停服务时发的正是这两类，收 SIGINT + SIGTERM 即可。
//
// 非 Windows：Ctrl+C → SIGINT，systemd / kill → SIGTERM，同一对。
func stopSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
