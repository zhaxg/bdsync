//go:build !windows

package main

import "runtime"

// processMem -- 非 Windows 平台暂不采集（Linux 版后续读 /proc/self/status）。
func processMem() (rss, peakRss int64, ok bool) { return 0, 0, false }

// runtimeMem -- GC 后的 Go 运行时内存分解。
type runtimeMem struct {
	HeapAlloc    int64
	HeapSys      int64
	HeapReleased int64
	Sys          int64
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

// liveHeap -- 存活堆。刻意**不**触发 GC：原实现会走 sampleMem 里的 runtime.GC()，
// 而 liveHeap 被 /api/status 每次轮询调用 ---- 等于让看板每隔一秒触发一次全量
// GC + STW。要"GC 后"的口径请显式用 sampleMem。
func liveHeap() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}
