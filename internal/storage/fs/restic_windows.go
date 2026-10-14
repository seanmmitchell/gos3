//go:build windows

package fs

import (
	"os/exec"
	"syscall"
)

func hideResticWindow(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }
