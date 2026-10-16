package fs

import (
	"context"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// resticWriter archives one object version into the bucket's repository
// while it is written (see restic.Archiver) and stores the snapshot ID as
// the version's data file. Empty objects never reach restic and have no
// data file. Not safe for concurrent use; the object writers serialize it.
type resticWriter struct {
	b    *Backend
	id   string // bucket ID
	a    *restic.Archiver
	size int64 // bytes written
}

// newResticWriter prepares the repository of the bucket with ID id (and
// the bucket key its password derives from, which a replica receiving its
// first copy does not have yet) and returns a writer archiving bucket/key
// into it.
func (b *Backend) newResticWriter(ctx context.Context, id, bucket, key string) (*resticWriter, error) {
	if _, err := b.bucketKey(id, true); err != nil {
		return nil, err
	}
	if err := b.prepareResticFor(ctx, id, bucket); err != nil {
		return nil, err
	}
	loc, err := b.resticLocation(id)
	if err != nil {
		return nil, err
	}
	return &resticWriter{b: b, id: id, a: b.resticRuntime().NewArchiverWith(ctx, loc, restic.ArchiveOptions{Bucket: bucket, BucketID: id, Key: key})}, nil
}

func (w *resticWriter) Write(p []byte) (int, error) {
	n, err := w.a.Write(p)
	w.size += int64(n)
	return n, err
}

// abort stops restic before its input ends, so no snapshot is saved.
func (w *resticWriter) abort() { w.a.Abort() }

// finish runs check (when not nil; e.g. the write's preconditions), then
// ends restic's input and waits for the snapshot. It returns the data ID
// and final path of the version's snapshot reference as finishData does
// ("" for an empty object, which has no snapshot). On any failure, check's
// included, nothing is archived.
func (w *resticWriter) finish(check func() error) (dataID, path string, err error) {
	snapshot, err := w.a.Finish(check)
	if err != nil || snapshot == "" {
		return "", "", err
	}
	// The local data file is an immutable pointer. No plaintext is kept.
	ref := w.b.newTemp()
	if _, err := ref.Write([]byte(snapshot)); err != nil {
		ref.discard()
		return "", "", err
	}
	return w.b.finishData(ref, w.id)
}

// checkWriteConditions evaluates the preconditions of opts for a new
// version of bucket/key before it is archived, so that a refused write
// costs no snapshot. commitObject evaluates them again atomically; a write
// racing in between can still leave an unreferenced snapshot.
func (b *Backend) checkWriteConditions(bucket, bucketID, key string, opts storage.CommitOptions) error {
	if !opts.IfNoneMatch && opts.IfMatch == "" {
		return nil
	}
	return b.view(func(tx *bbolt.Tx) error {
		_, bb, err := loadBucketID(tx, bucket, bucketID)
		if err != nil {
			return err
		}
		return checkPreconditions(bb, key, opts)
	})
}
