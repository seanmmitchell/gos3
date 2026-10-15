package storage

import "context"

// Per-bucket capabilities. A backing may hold buckets of different kinds
// (an s3 backing holds native and wrapped buckets, see BucketKind), so what
// a bucket supports cannot always be told from the backend's type: the
// multi router and the job runners ask through these interfaces instead.
//
// CONTRACT: exported names in this file are used by multi, the s3 backings
// and the job runners.

// BucketCaps describes what one bucket (or a bucket about to be created)
// supports.
type BucketCaps struct {
	// Kind is the bucket's kind ("" for backends without kinds).
	Kind BucketKind
	// Import: the bucket can receive imported versions (VersionImporter):
	// it can be a replica, a move target or a copy target keeping version
	// IDs and modification times.
	Import bool
	// History: the bucket's version history can be copied exactly (its
	// version IDs are valid imported IDs), so it can be the primary of
	// replicas or the source of a move.
	History bool
	// Clone: the bucket can be the source of a copy job.
	Clone bool
	// Versioning and ObjectLock: the bucket can be versioned / have object
	// lock enabled.
	Versioning bool
	ObjectLock bool
	// Compression: compressed writes are stored compressed.
	Compression bool
}

// fullCaps are the capabilities of a bucket on a backend that imports
// versions and has no bucket kinds (fs, memory).
var fullCaps = BucketCaps{Import: true, History: true, Clone: true, Versioning: true, ObjectLock: true, Compression: true}

// BucketCapabilities is implemented by backends whose buckets differ in
// what they support.
//
// BucketCaps reports the capabilities of an existing bucket
// (ErrBucketNotFound for an unknown one). NewBucketCaps reports those a
// bucket created with opts would have, without creating anything and
// without remote I/O; it returns ErrInvalidTarget (wrapped) when opts
// cannot be honoured, and must be consulted with opts.Import set before a
// bucket is created as a replica or move target.
type BucketCapabilities interface {
	BucketCaps(ctx context.Context, bucket string) (BucketCaps, error)
	NewBucketCaps(ctx context.Context, opts CreateBucketOptions) (BucketCaps, error)
}

// CapsOf returns the capabilities of bucket on b: those reported by a
// BucketCapabilities backend, or else full capabilities when b imports
// versions (VersionImporter) and none of Import/History otherwise. It does
// not check that the bucket exists on backends without
// BucketCapabilities.
func CapsOf(ctx context.Context, b Backend, bucket string) (BucketCaps, error) {
	if bc, ok := b.(BucketCapabilities); ok {
		return bc.BucketCaps(ctx, bucket)
	}
	return defaultCaps(b), nil
}

// NewCapsOf returns the capabilities a bucket created on b with opts would
// have (see BucketCapabilities.NewBucketCaps). Backends without
// BucketCapabilities refuse a non-nil opts.Target with ErrInvalidTarget.
func NewCapsOf(ctx context.Context, b Backend, opts CreateBucketOptions) (BucketCaps, error) {
	if bc, ok := b.(BucketCapabilities); ok {
		return bc.NewBucketCaps(ctx, opts)
	}
	if opts.Target != nil {
		return BucketCaps{}, ErrInvalidTarget
	}
	return defaultCaps(b), nil
}

func defaultCaps(b Backend) BucketCaps {
	if _, ok := b.(VersionImporter); ok {
		c := fullCaps
		if _, ok := b.(CompressingBackend); !ok {
			c.Compression = false
		}
		return c
	}
	return BucketCaps{Clone: true, Versioning: true, ObjectLock: true}
}

// BucketReleaser is implemented by backends that can forget a bucket
// without deleting its data (a native bucket on an s3 backing: the remote
// bucket and its objects stay). ReleaseBucket returns ErrNotSupported for
// buckets whose data would be orphaned.
type BucketReleaser interface {
	ReleaseBucket(ctx context.Context, name string) error
}

// HealthProber is implemented by backends with a cheaper or more accurate
// health check than ListBuckets. ProbeHealth returns an error wrapping
// ErrBackingUnavailable when the backing as a whole cannot serve.
type HealthProber interface {
	ProbeHealth(ctx context.Context) error
}

// KindReporter is implemented by backings that offer several bucket kinds:
// the kinds they offer and the one new buckets get by default.
type KindReporter interface {
	BucketKinds() (kinds []BucketKind, def BucketKind)
}
