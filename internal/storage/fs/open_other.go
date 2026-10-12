//go:build !windows

package fs

import "os"

// openShared opens path for reading. On POSIX systems an open file can
// always be unlinked or renamed, so this is os.Open.
func openShared(path string) (*os.File, error) { return os.Open(path) }

// openRaced reports whether an open failure can be a transient effect of a
// concurrent delete or replace other than the file being gone; never on
// POSIX systems, where that shows up as fs.ErrNotExist.
func openRaced(error) bool { return false }
