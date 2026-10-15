//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

// openBrowser -- 用系统默认浏览器打开地址。
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
