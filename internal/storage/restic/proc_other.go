//go:build !windows

package restic

import (
	"os"
	"os/exec"
	"time"
)

// hideWindow is a no-op outside Windows.
func hideWindow(*exec.Cmd) {}

// interrupt asks restic to stop: on SIGINT it removes its repository lock
// and exits without saving a snapshot.
func interrupt(p *os.Process) error { return p.Signal(os.Interrupt) }

// timeArg formats t for restic's --time, which restic parses in its local
// time zone: restic runs with the returned environment, TZ=UTC.
func timeArg(t time.Time) (string, []string) {
	return t.UTC().Format(resticTimeFormat), []string{"TZ=UTC"}
}
