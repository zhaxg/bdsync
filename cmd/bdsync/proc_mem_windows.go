//go:build windows

package main

import (
	"runtime"
	"syscall"
	"unsafe"
)

// PROCESS_MEMORY_COUNTERS（psapi.h）
type processMemoryCounters struct {
	Size                       uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var procGetProcessMemoryInfo = syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

const pseudoProcess = ^uintptr(0) // GetCurrentProcess()

// processMem -- 当前进程工作集（RSS）与峰值工作集。
func processMem() (rss, peakRss int64, ok bool) {
	var pmc processMemoryCounters
	r, _, _ := procGetProcessMemoryInfo.Call(
		pseudoProcess,
		uintptr(unsafe.Pointer(&pmc)),
		uintptr(uint32(unsafe.Sizeof(pmc))),
	)
	if r == 0 {
		return 0, 0, false
	}
	return int64(pmc.WorkingSetSize), int64(pmc.PeakWorkingSetSize), true
}

// runtimeMem -- GC 后的 Go 运行时内存分解（真实业务占用，不含待回收垃圾）。
type runtimeMem struct {
	HeapAlloc    int64 // 存活堆
	HeapSys      int64 // 堆从 OS 申请的总量
	HeapReleased int64 // 已归还 OS 的部分（HeapSys - 它 ~= 堆仍占着的）
	Sys          int64 // Go 运行时从 OS 申请的总量（含堆外）
	NumGC        uint32
}

func sampleMem() runtimeMem {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return runtimeMem{
		HeapAlloc:    int64(m.HeapAlloc),
		HeapSys:      int64(m.HeapSys),
		HeapReleased: int64(m.HeapReleased),
		Sys:          int64(m.Sys),
		NumGC:        m.NumGC,
	}
}

// liveHeap -- GC 后存活堆。
func liveHeap() int64 { return sampleMem().HeapAlloc }
