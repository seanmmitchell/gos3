//go:build !unix && !windows

package restic

import "testing"

// allocation cannot tell the disk space allocated to a file here.
func allocation(*testing.T, string) (allocated int64, sparse, known bool) { return 0, false, false }
