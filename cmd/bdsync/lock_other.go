//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// acquireSingleInstance -- 见 lock_windows.go 的说明。
// 非 Windows 用 flock(LOCK_EX|LOCK_NB) 实现，语义一致：锁随进程退出自动释放，
// 残留锁文件不影响下次启动（锁的持有状态由 flock 决定，而非文件是否存在）。
func acquireSingleInstance(target string) (func(), error) {
	path := target + ".lock"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件 %s 失败: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("另一个 bdsync 实例正在使用 %s（锁文件 %s）: %w", target, path, err)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	return func() { _ = f.Close() }, nil
}
