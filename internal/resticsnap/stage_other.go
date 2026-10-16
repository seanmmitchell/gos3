//go:build !windows

package resticsnap

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// checkPrivateDir checks that dir is a directory (not a link) owned by the
// server's user, and tightens a mode looser than 0700.
func checkPrivateDir(dir string, _ bool) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("not a directory")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("owned by user %d, not by the server's user %d", st.Uid, os.Getuid())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}
