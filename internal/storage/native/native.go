// Package native stores the objects of an s3 backing's native buckets: a
// gos3 bucket bound to one remote S3 bucket (adopted or created by gos3)
// whose objects are stored as-is, so other S3 clients keep reading and
// writing them (see docs/backings.md, "Bucket kinds").
//
// Versioning and object lock are the remote bucket's: version IDs and
// delete markers are the remote's, retention and legal holds are set with
// the remote's object lock headers and APIs, and gos3 changes the remote
// bucket's versioning and object lock configuration on request. gos3
// metadata the remote cannot hold travels in the x-amz-meta-gos3-meta
// header written with the object; the ACL and the website redirect, which
// anybody who can write the remote bucket could otherwise set, and
// changes made after the object was written are kept in overlay records
// in the StateStore, bound to the exact remote version (never by copying
// the object onto itself, which would create remote versions).
//
// The bucket records and configuration documents belong to the caller
// (internal/storage/s3account): a Store only handles objects, multipart
// uploads and the remote bucket's own settings, and learns how a gos3
// bucket maps to a remote bucket through Options.Resolve.
//
// CONTRACT: the exported names in this file are used by s3account; the
// Store's object and multipart methods have exactly the signatures and
// semantics of the storage.Backend methods of the same names (and of
// storage.ConditionalDeleter, CheckedDeleter, ConditionalMetaUpdater and
// UploadWalker).
package native

import (
	"context"
	"io"
	"log/slog"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Options configures Open.
type Options struct {
	// Name is the backing name (errors and logs).
	Name string
	// Client talks to the remote service in the backing's region. Required.
	// Buckets in other regions are reached with Client.ForRegion.
	Client *s3client.Client
	// State keeps multipart upload state and overlay records. Required.
	State StateStore
	// Resolve returns the binding and recorded settings of a gos3 bucket.
	// It is called for every operation (so a change of versioning takes
	// effect at once) and must be cheap (an in-memory lookup). It returns
	// an error wrapping storage.ErrBucketNotFound for unknown buckets.
	Resolve func(ctx context.Context, bucket string) (Bucket, error)
	// OnDrift is called (asynchronously safe, must not block) when an
	// answer of the remote contradicts the recorded settings of a bucket
	// (a version ID from a bucket recorded as unversioned, refused object
	// lock headers): the caller should Refresh the bucket. nil ignores it.
	OnDrift func(bucket, reason string)
	// PartSize is the upload part size and the largest object sent with a
	// single PUT (0 = s3client.DefaultPartSize).
	PartSize int64
	// SpoolDir holds temporary files buffering uploads ("" = os.TempDir()).
	// When set it is dedicated to this store: stale spool files in it are
	// removed by Open.
	SpoolDir string
	// Logger receives non-fatal errors. nil discards logs.
	Logger *slog.Logger
	// ForbidCompliance refuses COMPLIANCE-mode retention with
	// storage.ErrNotSupported (tests against real services).
	ForbidCompliance bool
}

// Binding is where a native bucket lives.
type Binding struct {
	// Remote is the remote bucket's name and Region its region ("" = the
	// client's region).
	Remote string `json:"remote"`
	Region string `json:"region,omitempty"`
	// Owned is set when gos3 created the remote bucket: deleting the gos3
	// bucket deletes the (empty) remote bucket too. Adopted remote buckets
	// are never deleted.
	Owned bool `json:"owned,omitempty"`
}

// Bucket is what Options.Resolve reports for a gos3 bucket: its binding
// and the remote settings as last recorded (see RemoteState).
type Bucket struct {
	Binding
	Versioning storage.VersioningStatus
	ObjectLock bool
}

// Caps are the optional features a remote bucket supports, as probed by
// AttachBucket/Refresh or learned from 501 answers.
type Caps struct {
	Versioning bool `json:"versioning"`
	ObjectLock bool `json:"objectLock"`
	Tagging    bool `json:"tagging"`
}

// RemoteState is a remote bucket's state as read from the service.
type RemoteState struct {
	Binding
	Versioning storage.VersioningStatus
	ObjectLock bool
	// LockConfig is the remote's object lock configuration as an S3
	// ObjectLockConfiguration XML document (nil when it has none), so the
	// caller can store it as the bucket's storage.ConfigObjectLock.
	LockConfig []byte
	Caps       Caps
}

// AttachIntent says whether AttachBucket creates or adopts the remote
// bucket.
type AttachIntent int

const (
	// Create creates a new remote bucket; an existing one is refused with
	// storage.ErrRemoteBucketExists.
	Create AttachIntent = iota
	// Adopt binds an existing remote bucket; a missing one is refused with
	// an error wrapping storage.ErrInvalidTarget.
	Adopt
)

// AttachRequest asks AttachBucket to bind gos3 bucket Bucket to the remote
// bucket Remote.
type AttachRequest struct {
	Bucket string
	Remote string
	Region string // "" = discover (Adopt) or the client's region (Create)
	Intent AttachIntent
	// ObjectLock creates the remote bucket with object lock enabled
	// (Create only).
	ObjectLock bool
}

// StateStore keeps the store's state outside the process: multipart
// upload records, staged small parts and overlay records. In the server it
// is an aux namespace of the backing's home (s3store.AuxState) or a local
// state database (internal/storage/localstate).
//
// Every record is loaded by Open; the in-memory state is authoritative
// while the store is open and every change is written through.
//
// All methods must be safe for concurrent use.
type StateStore interface {
	// Records returns every record (key -> value). It is called by Open.
	Records(ctx context.Context) (map[string][]byte, error)
	// PutRecord creates or replaces a record. It must be durable when it
	// returns nil; an error leaves the outcome unknown (the write may
	// have landed), and the store copes with either.
	PutRecord(ctx context.Context, key string, val []byte) error
	// DeleteRecords deletes every record whose key starts with prefix
	// (none is not an error).
	DeleteRecords(ctx context.Context, prefix string) error
	// PutStaged stores the size bytes of r as the staged blob name. It
	// must be durable when it returns nil.
	PutStaged(ctx context.Context, name string, r io.ReaderAt, size int64) error
	// OpenStaged opens a staged blob of the given size for reading.
	OpenStaged(ctx context.Context, name string, size int64) (io.ReadSeekCloser, error)
	// DeleteStaged deletes a staged blob (a missing one is not an error).
	DeleteStaged(ctx context.Context, name string) error
	// ListStaged returns the names of every staged blob, for the removal
	// of blobs no record refers to at Open.
	ListStaged(ctx context.Context) ([]string, error)
}

// Bucket-level operations of a Store (implemented in buckets.go):
//
//	AttachBucket(ctx, AttachRequest) (RemoteState, error)
//	    Refuse remote buckets marked as gos3 stores (gos3.json at the root)
//	    and remote buckets already bound to another gos3 bucket of this
//	    store; discover the region; create (Create) or require (Adopt) the
//	    remote bucket; probe versioning and object lock.
//	Refresh(ctx, bucket) (RemoteState, error)
//	    Re-read the remote bucket's state (and region).
//	SetVersioning(ctx, bucket, storage.VersioningStatus) error
//	EnableObjectLock(ctx, bucket) error
//	PutObjectLockConfig(ctx, bucket, xml []byte) error
//	    Mirror the default retention of an ObjectLockConfiguration document
//	    to the remote bucket.
//	CheckEmpty(ctx, bucket) error
//	    storage.ErrBucketNotEmpty while the remote bucket holds objects,
//	    versions or delete markers.
//	DetachBucket(ctx, bucket, removeRemote bool) error
//	    Abort gos3's uploads, drop overlay records; with removeRemote
//	    (Owned bindings) delete the empty remote bucket.
//	Close() error
//
// Remote failures map to storage errors as documented in errors.go:
// failures of one remote bucket (NoSuchBucket, bucket-level 403, wrong
// region) wrap storage.ErrBucketUnavailable, never just
// storage.ErrBackingUnavailable, which is kept for transport failures and
// 5xx answers.
