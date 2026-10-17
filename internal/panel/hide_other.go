//go:build !windows

package panel

import "os/exec"

func setHidden(cmd *exec.Cmd) {}
