//go:build !windows

package fs

import (
	"os"
	"os/exec"
)

// hideResticWindow is a no-op outside Windows.
func hideResticWindow(*exec.Cmd) {}

// interruptRestic asks restic to stop: on SIGINT it removes its repository
// lock and exits without saving a snapshot.
func interruptRestic(p *os.Process) error { return p.Signal(os.Interrupt) }
