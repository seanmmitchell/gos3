//go:build !unix && !windows

package server

import "os"

// lockFile only creates path: this platform has no file locking.
func lockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}
