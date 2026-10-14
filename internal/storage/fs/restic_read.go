package fs

import (
	"context"
	"fmt"
	"io"
	"os"
)

// openRestic returns the reader of a restic version of size bytes in the
// bucket with ID id whose snapshot reference is f (closed here). Nothing
// is restored until the reader is read or prepared (see restic.Reader:
// concurrent readers of a snapshot share one restore, spooled encrypted in
// tmp/).
func (b *Backend) openRestic(ctx context.Context, f *os.File, id string, size int64) (io.ReadSeekCloser, error) {
	defer f.Close()
	ref, err := io.ReadAll(io.LimitReader(f, snapshotRefSize+1))
	if err != nil {
		return nil, err
	}
	if !validSnapshot(string(ref)) {
		return nil, fmt.Errorf("%w: invalid restic snapshot reference", errCorrupt)
	}
	loc, err := b.resticLocation(id)
	if err != nil {
		return nil, err
	}
	return b.resticRuntime().Open(ctx, loc, string(ref), size)
}
