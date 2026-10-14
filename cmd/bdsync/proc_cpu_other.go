//go:build !windows

package main

import "time"

// processCPUTime — 非 Windows 平台暂不采集（Linux 版后续用 getrusage）。
func processCPUTime() (time.Duration, bool) { return 0, false }
