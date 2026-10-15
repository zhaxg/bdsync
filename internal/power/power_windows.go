//go:build windows

// Package power -- Windows 进程静默控制（design §4.4/§4.5）：
// 优先级切换、Job Object CPU 硬上限、内存修剪。
package power

import (
	"runtime"
	"runtime/debug"
	"syscall"
	"unsafe"
)

var (
	k32                          = syscall.NewLazyDLL("kernel32.dll")
	procSetPriorityClass         = k32.NewProc("SetPriorityClass")
	procCreateJobObjectW         = k32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject = k32.NewProc("AssignProcessToJobObject")
	procSetInformationJobObject  = k32.NewProc("SetInformationJobObject")
	procSetProcessWorkingSetSize = k32.NewProc("SetProcessWorkingSetSize")
)

// 优先级类（winbase.h）
const (
	idlePriorityClass   = 0x00000040
	belowNormalPriority = 0x00004000
	normalPriorityClass = 0x00000020
)

// Job Object CPU 速率控制
const (
	jobObjectCpuRateControlInformation = 3
	cpuRateControlEnable               = 0x1
	cpuRateControlHardCap              = 0x4
)

// currentProcess -- 伪句柄，无需 CloseHandle。
const currentProcess = ^uintptr(0)

var (
	jobHandle    uintptr
	quietCapPct  = 5  // quota.quiet_cpu_cap（config 覆盖）
	activeCapPct = 80 // quota.active_cpu_cap
)

// ConfigureCaps -- CPU 硬上限（%，来自配置）。
func ConfigureCaps(quiet, active int) {
	if quiet > 0 && quiet <= 100 {
		quietCapPct = quiet
	}
	if active > 0 && active <= 100 {
		activeCapPct = active
	}
}

// InitJob -- 创建 Job 并把当前进程放进去（CPU 硬上限的前置）。
// 进程已在别的 Job 里（如容器）时会失败 ---- 非致命，返回错误即可。
func InitJob() error {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return err
	}
	r, _, _ := procAssignProcessToJobObject.Call(h, currentProcess)
	if r == 0 {
		return err
	}
	jobHandle = h
	return nil
}

type jobCPURateInfo struct {
	ControlFlags uint32
	CpuRate      uint32 // 1/100 百分比：100=1%，10000=100%
}

func setCPUCap(pct int) {
	if jobHandle == 0 || pct <= 0 {
		return
	}
	if pct > 100 {
		pct = 100
	}
	info := jobCPURateInfo{
		ControlFlags: cpuRateControlEnable | cpuRateControlHardCap,
		CpuRate:      uint32(pct) * 100,
	}
	procSetInformationJobObject.Call(
		jobHandle,
		jobObjectCpuRateControlInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
}

// SetQuiet -- 空闲态：IDLE 优先级 + 硬上限（默认 5%，design §4.4）。
// 永不使用 REALTIME（design §4.5 铁律）。
func SetQuiet() {
	procSetPriorityClass.Call(currentProcess, idlePriorityClass)
	setCPUCap(quietCapPct)
}

// SetActive -- 活动态：NORMAL 优先级 + 硬上限（默认 80%，留 20% 给前台）。
func SetActive() {
	procSetPriorityClass.Call(currentProcess, normalPriorityClass)
	setCPUCap(activeCapPct)
}

// SetBelowNormal -- 中间档（可选）。
func SetBelowNormal() {
	procSetPriorityClass.Call(currentProcess, belowNormalPriority)
	setCPUCap(50)
}

// TrimMemory -- 归还内存给 OS（design §4.4：只在进入 QUIET 时调用，成本不小）。
func TrimMemory() {
	runtime.GC()
	debug.FreeOSMemory()
	procSetProcessWorkingSetSize.Call(currentProcess, ^uintptr(0), ^uintptr(0))
}
