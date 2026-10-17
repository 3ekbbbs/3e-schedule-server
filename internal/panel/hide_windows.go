//go:build windows

package panel

import (
	"os/exec"
	"syscall"
)

// setHidden 让子进程不弹控制台窗口。
func setHidden(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
