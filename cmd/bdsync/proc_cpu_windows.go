//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"
)

var procGetProcessTimes = syscall.NewLazyDLL("kernel32.dll").NewProc("GetProcessTimes")

// processCPUTime — 当前进程累计 CPU 时间（kernel + user）。
// GetCurrentProcess() 伪句柄 = ^uintptr(0)。
func processCPUTime() (time.Duration, bool) {
	const pseudoHandle = ^uintptr(0)
	var creation, exit, kernel, user syscall.Filetime
	r, _, _ := procGetProcessTimes.Call(
		pseudoHandle,
		uintptr(unsafe.Pointer(&creation)),
		uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return 0, false
	}
	return filetimeDur(kernel) + filetimeDur(user), true
}

// filetimeDur — FILETIME(100ns 单位) → time.Duration。
func filetimeDur(ft syscall.Filetime) time.Duration {
	ticks := int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
	return time.Duration(ticks * 100)
}
