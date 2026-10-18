package s3account

import (
	"context"
	"errors"
	"fmt"

	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Restic repositories (restic.Host)
//
//	ResticRepository     wrapped bucket: its container's repository of the
//	                     bucket (<container>restic/<bucket-id>)
//	                     native bucket: errors.ErrUnsupported (it keeps no
//	                     repository; whole-bucket snapshots of it go to an
//	                     aux repository)
//	ResticAuxRepository  the home's aux repository (<home>restic-aux/<name>,
//	                     keyed by a key wrapped with the backing's
//	                     encryption_key); errors.ErrUnsupported without a
//	                     home (records kept locally) or without an
//	                     encryption key
//
// Aux repositories of backings without a home (whose records are in the
// local system directory) are not supported yet: snapshots of their native
// buckets need another target (an fs backing, or a backing with a home).

var _ restic.Host = (*Backend)(nil)

// ResticRepository implements restic.Host: a wrapped bucket's repository
// is its container's; a native bucket keeps none (an error wrapping
// errors.ErrUnsupported). Unknown buckets are storage.ErrBucketNotFound.
func (b *Backend) ResticRepository(ctx context.Context, bucket string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	if t.native() {
		return restic.RepositoryInfo{}, restic.Location{}, fmt.Errorf(
			"s3account: bucket %q is a native bucket (stored as-is in a remote bucket), which keeps no restic repository: %w",
			bucket, errors.ErrUnsupported)
	}
	info, loc, err := t.c.store.ResticRepository(ctx, bucket, create)
	return info, loc, t.c.classify(err)
}

// ResticAuxRepository implements restic.Host: the aux repository name of
// the backing's home (see s3store.Backend.ResticAuxRepository). A backing
// without a home answers an error wrapping errors.ErrUnsupported, as does
// one without an encryption key.
func (b *Backend) ResticAuxRepository(ctx context.Context, name string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	if err := b.serving(ctx); err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	if b.home == nil {
		return restic.RepositoryInfo{}, restic.Location{}, fmt.Errorf(
			"s3account: s3 backing %q has no home (its records are kept locally), which aux restic repositories need: %w",
			b.name, errors.ErrUnsupported)
	}
	return b.home.ResticAuxRepository(ctx, name, create)
}
