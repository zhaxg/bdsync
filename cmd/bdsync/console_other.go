//go:build !windows

package main

import "os"

func init() {} // 非 Windows 控制台默认按 UTF-8 处理，无需干预

// flushConsole -- 无转码管道，无需收尾（见 console_windows.go）。
func flushConsole() {}

// exitProcess -- os.Exit 的直接替身（非 Windows 没有转码管道要收尾）。
func exitProcess(code int) { os.Exit(code) }
