//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps the child ffmpeg from opening its own console window.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
