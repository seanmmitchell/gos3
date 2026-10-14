//go:build !windows

package fs

import "os/exec"

func hideResticWindow(*exec.Cmd) {}
