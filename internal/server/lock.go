package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LockFileName is the file in the system directory that a running server
// holds an exclusive lock on.
const LockFileName = "lock"

// errLocked is returned by lockFile when another process holds the lock.
var errLocked = errors.New("locked by another process")

// lockSystemDir takes an exclusive lock on <systemDir>/lock so that only one
// gos3 process uses a data directory at a time, whatever the backend: with
// the memory backend nothing else would stop two servers from rewriting each
// other's iam.json and settings.json. The lock is released when the
// returned file is closed (or the process exits).
func lockSystemDir(systemDir string) (*os.File, error) {
	path := filepath.Join(systemDir, LockFileName)
	f, err := lockFile(path)
	if errors.Is(err, errLocked) {
		return nil, fmt.Errorf("data directory %s is in use (is another gos3 using this data directory?)", filepath.Dir(systemDir))
	}
	if err != nil {
		return nil, fmt.Errorf("locking the data directory: %w", err)
	}
	return f, nil
}
