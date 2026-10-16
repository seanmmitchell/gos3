package restic

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// placeholderDirPerm is the mode of directories MakePlaceholder creates.
const placeholderDirPerm = 0o700

// MakePlaceholder makes path (creating its directory, or replacing a file
// that is there) a file of size zero bytes with the modification time
// mtime: a sparse file, which occupies no disk space, where the file
// system supports them (Windows: FSCTL_SET_SPARSE on NTFS and ReFS;
// elsewhere extending a file creates a hole), and an allocated one
// otherwise.
//
// Staged for BackupTree with TreeOptions.IgnoreInode and IgnoreCtime, a
// placeholder of a file of the parent snapshot, with that file's size and
// modification time, is not read: the new snapshot keeps the parent's
// content. mtime should come from the parent snapshot (or a Stat of the
// file system), so that the file system stores it exactly (Windows keeps
// 100 ns).
func MakePlaceholder(path string, size int64, mtime time.Time) error {
	if size < 0 {
		return fmt.Errorf("restic: invalid placeholder size %d", size)
	}
	if err := os.MkdirAll(filepath.Dir(path), placeholderDirPerm); err != nil {
		return fmt.Errorf("restic: placeholder: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("restic: placeholder: %w", err)
	}
	if size > 0 {
		// Best effort: without sparse files the zeros are allocated.
		_ = setSparse(f)
		if err := f.Truncate(size); err != nil {
			_ = f.Close()
			return fmt.Errorf("restic: placeholder: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("restic: placeholder: %w", err)
	}
	// The access time stays as it is.
	if err := os.Chtimes(path, time.Time{}, mtime); err != nil {
		return fmt.Errorf("restic: placeholder: %w", err)
	}
	return nil
}
