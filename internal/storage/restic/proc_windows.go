//go:build windows

package restic

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// hideWindow keeps restic from opening a console window.
func hideWindow(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

// interrupt stops restic. Windows has no SIGINT for another process, so it
// is killed; a stale lock it leaves behind does not block backups, and
// restic ignores it once it is recognized as stale.
func interrupt(p *os.Process) error { return p.Kill() }

// timeArg formats t for restic's --time, which restic parses in its local
// time zone. Windows programs ignore TZ, so t is formatted in the system's
// time zone, which restic shares (a time in the hour repeated when daylight
// saving time ends may be read an hour off).
func timeArg(t time.Time) (string, []string) {
	return t.In(time.Local).Format(resticTimeFormat), nil
}
