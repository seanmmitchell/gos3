//go:build windows

package restic

import (
	"os"

	"golang.org/x/sys/windows"
)

// setSparse marks f sparse, so that extending it allocates nothing.
func setSparse(f *os.File) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioErr error
	err = conn.Control(func(fd uintptr) {
		var n uint32
		ioErr = windows.DeviceIoControl(windows.Handle(fd), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &n, nil)
	})
	if err != nil {
		return err
	}
	return ioErr
}
