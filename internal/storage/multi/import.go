package multi

import (
	"context"
	"errors"
	"fmt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// The router imports versions (storage.VersionImporter and
// storage.CompressingImporter) into a bucket's primary, for copy jobs that
// keep version IDs and modification times: like an object write, the
// version goes to the primary and, in an active-active bucket, to every
// in-sync replica before it is acknowledged (see fanout), and the bucket's
// other copies receive it in the background. A bucket whose primary cannot
// import (a native bucket of an s3 backing, a backend without
// storage.VersionImporter) refuses with an error wrapping both
// errors.ErrUnsupported and storage.ErrNotSupported, so that callers fall
// back to plain writes.

// versionWriter is the router's storage.VersionWriter (see fanout).
type versionWriter struct {
	*fanout
	w storage.VersionWriter
}

// NewVersionWriter implements storage.VersionImporter (see
// NewVersionWriterWith).
func (r *Router) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	return r.NewVersionWriterWith(ctx, bucket, key, storage.WriteOptions{})
}

// NewVersionWriterWith implements storage.CompressingImporter: it starts an
// imported version of bucket/key on the bucket's primary and on its
// in-sync active-active replicas, stored as opts asks where the backing
// can (compression) or must (encryption, refused otherwise). It passes the
// bucket's gate like an object write and holds it until Commit or Abort.
// A primary that cannot import versions yields an error wrapping
// errors.ErrUnsupported and storage.ErrNotSupported.
func (r *Router) NewVersionWriterWith(ctx context.Context, bucket, key string, opts storage.WriteOptions) (storage.VersionWriter, error) {
	if err := checkCompression(opts.Compression); err != nil {
		return nil, err
	}
	if err := r.checkWriteEncryption(ctx, bucket, opts); err != nil {
		return nil, err
	}
	rt, leave, err := r.enterWrite(ctx, bucket)
	if err != nil {
		return nil, err
	}
	caps, err := storage.CapsOf(ctx, rt.lead.Backend, bucket)
	if err != nil {
		leave()
		return nil, r.observe(rt.lead, err)
	}
	if !caps.Import {
		leave()
		return nil, errNoImportInto(rt.lead, bucket, caps.Kind, nil)
	}
	w, err := newVersionWriter(ctx, rt.lead, bucket, key, opts)
	if err != nil {
		leave()
		if errors.Is(err, errors.ErrUnsupported) || errors.Is(err, storage.ErrNotSupported) {
			return nil, errNoImportInto(rt.lead, bucket, caps.Kind, err)
		}
		return nil, r.observe(rt.lead, err)
	}
	return &versionWriter{fanout: r.newFanout(ctx, rt, leave, bucket, key, w, opts), w: w}, nil
}

// Commit stores the version on the primary with exactly info's identity
// (see storage.VersionWriter), then on the tees with the info the primary
// stored.
func (w *versionWriter) Commit(ctx context.Context, info storage.ObjectInfo) (storage.ObjectInfo, error) {
	return w.commit(ctx, nil, func() (storage.ObjectInfo, error) {
		return w.w.Commit(ctx, info)
	})
}

// errNoImportInto is the error for bucket, whose primary on backing b (a
// bucket of kind) cannot receive imported versions; cause is the backing's
// refusal, if any. It wraps errors.ErrUnsupported and
// storage.ErrNotSupported.
func errNoImportInto(b *backing, bucket string, kind storage.BucketKind, cause error) error {
	var err error
	switch {
	case cause != nil:
		err = fmt.Errorf("multi: importing into bucket %q on backing %q: %w", bucket, b.Name, cause)
	case kind == storage.BucketKindNative:
		err = fmt.Errorf("multi: bucket %q on backing %q is a native bucket (stored as-is in a remote bucket), "+
			"which cannot receive imported versions", bucket, b.Name)
	default:
		err = fmt.Errorf("multi: bucket %q on backing %q cannot receive imported versions", bucket, b.Name)
	}
	if !errors.Is(err, storage.ErrNotSupported) {
		err = fmt.Errorf("%w: %w", err, storage.ErrNotSupported)
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		err = fmt.Errorf("%w: %w", err, errors.ErrUnsupported)
	}
	return err
}
