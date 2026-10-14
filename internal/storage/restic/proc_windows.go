//go:build windows

package restic

import (
	"os"
	"os/exec"
	"syscall"
)

// hideWindow keeps restic from opening a console window.
func hideWindow(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

// interrupt stops restic. Windows has no SIGINT for another process, so it
// is killed; a stale lock it leaves behind does not block backups, and
// restic ignores it once it is recognized as stale.
func interrupt(p *os.Process) error { return p.Kill() }
