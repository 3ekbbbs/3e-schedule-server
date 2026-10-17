//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// openBrowser 用系统默认浏览器打开面板。
//
// 走 rundll32 而不是 `cmd /c start`：后者会闪一个控制台窗口。
func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}
