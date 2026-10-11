//go:build windows

package fs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// openShared opens path for reading. Unlike os.Open it allows other handles
// to delete or rename the file while it is open (FILE_SHARE_DELETE), which
// gives the POSIX semantics the backend relies on: replacing or deleting a
// version unlinks its data file even while readers hold it, and they keep
// reading the old content.
func openShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// openRaced reports whether an open failure can mean that the file was being
// deleted or replaced at that moment (a delete-pending file answers
// ERROR_ACCESS_DENIED, a conflicting handle ERROR_SHARING_VIOLATION), so the
// caller should re-resolve the metadata and try again.
func openRaced(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// longPath adds the \\?\ prefix to long absolute paths, as os.Open does, so
// data directories nested deeper than MAX_PATH keep working.
func longPath(path string) string {
	if len(path) < 248 || !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) {
		return path
	}
	return `\\?\` + filepath.Clean(path)
}
