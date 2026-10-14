//go:build !windows

package main

func init() {} // 非 Windows 控制台默认按 UTF-8 处理，无需干预
