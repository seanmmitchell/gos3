//go:build windows

package restic

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetCompressedFileSize = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

// allocation returns the disk space allocated to the file path and whether
// it is marked sparse.
func allocation(t *testing.T, path string) (allocated int64, sparse, known bool) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil {
		t.Fatal(err)
	}
	var high uint32
	low, _, callErr := procGetCompressedFileSize.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xFFFFFFFF && callErr != windows.ERROR_SUCCESS {
		t.Fatal(callErr)
	}
	return int64(high)<<32 | int64(uint32(low)), attrs&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0, true
}
