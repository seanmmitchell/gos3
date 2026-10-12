//go:build windows

package server

import (
	"errors"
	"os"
	"syscall"
)

// errorSharingViolation is ERROR_SHARING_VIOLATION (not defined by syscall).
const errorSharingViolation syscall.Errno = 32

// lockFile opens (creating) path without sharing: a second open fails with
// ERROR_SHARING_VIOLATION while the file is open.
func lockFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, errLocked
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
