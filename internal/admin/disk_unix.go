//go:build linux || darwin

package admin

import "syscall"

// diskUsage reports the size of the file system holding path (nil if it
// cannot be determined).
func diskUsage(path string) *diskStats {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil
	}
	bsize := uint64(st.Bsize)
	total := uint64(st.Blocks) * bsize
	free := uint64(st.Bavail) * bsize
	var used uint64
	if st.Blocks >= st.Bfree {
		used = (uint64(st.Blocks) - uint64(st.Bfree)) * bsize
	}
	return &diskStats{Path: path, TotalBytes: total, FreeBytes: free, UsedBytes: used}
}
