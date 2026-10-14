//go:build windows

package main

import "os/exec"

// openBrowser — 用系统默认浏览器打开地址。
// 走 rundll32 而不是 `cmd /c start`：不弹第二个黑框，双击场景更干净。
func openBrowser(url string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
