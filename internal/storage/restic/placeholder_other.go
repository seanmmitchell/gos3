//go:build !windows

package restic

import "os"

// setSparse does nothing: extending a file creates a hole, which reads as
// zeros, where the file system supports sparse files.
func setSparse(*os.File) error { return nil }
