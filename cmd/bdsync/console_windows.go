//go:build windows

package main

import "syscall"

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	setConsoleOutputCPProc = kernel32.NewProc("SetConsoleOutputCP")
	setConsoleCPProc       = kernel32.NewProc("SetConsoleCP")
)

// init — 把控制台切到 UTF-8 代码页。
//
// 启动器是纯 ASCII 的 .cmd（任何代码页都正常），所有中文由本程序输出；
// 若控制台仍是 GBK，这些 UTF-8 字节会显示成乱码 —— 这里一次性解决。
func init() {
	const cpUTF8 = 65001
	setConsoleOutputCPProc.Call(uintptr(cpUTF8))
	setConsoleCPProc.Call(uintptr(cpUTF8))
}
