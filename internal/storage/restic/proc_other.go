//go:build !windows

package restic

import (
	"os"
	"os/exec"
)

// hideWindow is a no-op outside Windows.
func hideWindow(*exec.Cmd) {}

// interrupt asks restic to stop: on SIGINT it removes its repository lock
// and exits without saving a snapshot.
func interrupt(p *os.Process) error { return p.Signal(os.Interrupt) }
