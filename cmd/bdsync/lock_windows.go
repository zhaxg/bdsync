//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// acquireSingleInstance -- 用"独占打开锁文件"保证同一索引库只有一个进程在用。
//
// 为什么不能只靠看板端口占用：那种判断只在 web.enabled=true 时成立；关掉看板
// 或走 daemon 子命令时，两个进程会同时打开同一个 index.db，产生重复上传与
// 状态互相覆盖（后写入者赢），且没有明显症状。
//
// Windows 上以 share mode = 0 打开：其他进程再打开同一文件必然失败。句柄由内核
// 在进程退出（含崩溃）时释放，因此**不需要**读 PID、也不需要清理残留锁文件 ----
// 锁的持有状态由句柄决定，而不是由文件是否存在决定。
func acquireSingleInstance(target string) (func(), error) {
	path := target + ".lock"
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("锁文件路径非法 %s: %w", path, err)
	}
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // 不共享：第二个实例打开会直接失败
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, fmt.Errorf("另一个 bdsync 实例正在使用 %s（锁文件 %s）: %w", target, path, err)
	}
	f := os.NewFile(uintptr(h), path)
	// 写入 PID 只为排障时能看出是谁占着（不参与判定）
	_ = f.Truncate(0)
	_, _ = f.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	return func() { _ = f.Close() }, nil
}
