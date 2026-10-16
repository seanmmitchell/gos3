//go:build unix

package restic

import (
	"os"
	"syscall"
	"testing"
)

// allocation returns the disk space allocated to the file path (sparse
// files have no flag outside Windows).
func allocation(t *testing.T, path string) (allocated int64, sparse, known bool) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false, false
	}
	return int64(st.Blocks) * 512, true, true
}
