//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

func openBrowser(url string) {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	_ = exec.Command(bin, url).Start()
}
