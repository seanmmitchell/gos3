//go:build windows

package fs

import (
	"os"
	"os/exec"
	"syscall"
)

// hideResticWindow keeps restic from opening a console window.
func hideResticWindow(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

// interruptRestic stops restic. Windows has no SIGINT for another process,
// so it is killed; a stale lock it leaves behind does not block backups,
// and restic ignores it once it is recognized as stale.
func interruptRestic(p *os.Process) error { return p.Kill() }
