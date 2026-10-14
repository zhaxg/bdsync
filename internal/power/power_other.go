//go:build !windows

// Package power — 非 Windows 平台的静默控制占位。
// Linux 版对应物：Nice=10 + IOSchedulingClass=idle + CPUQuota（systemd，design §4.10），
// 由部署配置承担，进程内不重复实现。
package power

import (
	"runtime"
	"runtime/debug"
)

func InitJob() error                  { return nil }
func ConfigureCaps(quiet, active int) {}
func SetQuiet()                       {}
func SetActive()                      {}
func SetBelowNormal()                 {}

// TrimMemory — 归还内存给 OS。
func TrimMemory() {
	runtime.GC()
	debug.FreeOSMemory()
}
