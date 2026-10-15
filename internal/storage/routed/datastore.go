package routed

import (
	"context"
	"io"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// DataStore holds the objects and multipart uploads of the buckets of a
// gateway backing (internal/storage/gateway), while the primary backend
// keeps the bucket records and bucket configuration documents. The router
// calls a DataStore only for buckets that exist in the primary, so
// implementations need not (and cannot reliably) check bucket existence
// themselves.
//
// The object and multipart methods have exactly the signatures and
// semantics of the storage.Backend methods of the same names (see
// internal/storage/storage.go and optional.go), restricted to what the
// store supports: an operation it cannot perform returns an error wrapping
// storage.ErrNotSupported, and failures to reach the remote service wrap
// storage.ErrBackingUnavailable.
//
// All methods must be safe for concurrent use.
type DataStore interface {
	// AttachBucket prepares the store for a new bucket (create or verify
	// the remote bucket or prefix) before the primary records it. info is
	// the bucket as it will be created (Name, Region, Owner, Backing). An
	// error aborts the bucket creation.
	AttachBucket(ctx context.Context, info storage.BucketInfo) error
	// CheckEmpty returns an error wrapping storage.ErrBucketNotEmpty when
	// the bucket still holds objects (in-progress uploads do not count).
	CheckEmpty(ctx context.Context, bucket string) error
	// DetachBucket is called after the primary deleted the bucket: it
	// aborts the bucket's in-progress uploads and drops any state kept for
	// it. Remote data and remote buckets are left alone.
	DetachBucket(ctx context.Context, bucket string) error

	NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error)
	GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error)
	OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error)
	UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error)
	DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error)
	DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error)
	WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error
	WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error

	CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error)
	GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error)
	NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error)
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error)
	ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error)
	WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error
	CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error

	// Close releases the store's resources.
	Close() error
}
