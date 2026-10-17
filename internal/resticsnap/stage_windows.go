package resticsnap

import (
	"errors"
	"io/fs"
	"os"

	"github.com/seanmmitchell/gos3/internal/config"
)

// checkPrivateDir checks that dir is a directory (not a link or junction)
// and, with restrict, gives it the system directory's ACL: the server's
// user, SYSTEM and Administrators only, inherited by its content.
func checkPrivateDir(dir string, restrict bool) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode().Type() != fs.ModeDir {
		return errors.New("not a directory")
	}
	if restrict {
		return config.RestrictDir(dir)
	}
	return nil
}
