package storage

import (
	"context"
	"time"
)

// Optional Backend extensions. They are additive to the Backend contract:
// callers discover them with a type assertion and fall back to the plain
// Backend methods (or refuse to act, where the fallback would be unsafe)
// when a backend does not implement them. Both built-in backends implement
// all of them.

// ConditionalDeleter makes a delete conditional, atomically.
//
// DeleteObjectIf looks up the object version addressed by versionID ("" =
// the key's latest version, which may be a delete marker) and calls check
// with it inside the same transaction (or under the same lock) as the
// delete. The delete, with exactly the semantics of Backend.DeleteObject
// for the same arguments, happens only when check returns nil; check's
// error is returned unchanged otherwise. A missing key or version yields
// ErrObjectNotFound / ErrVersionNotFound without calling check (also for
// versionID "" in an Unversioned bucket, where DeleteObject would succeed).
//
// check must not do I/O or call into the backend, and it may be called
// more than once (the last call decides).
type ConditionalDeleter interface {
	DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(ObjectInfo) error) (DeleteResult, error)
}

// CheckedDeleter is ConditionalDeleter for preconditions that may also be
// satisfied by the key having no object at all.
//
// DeleteObjectChecked is DeleteObjectIf, except that when versionID is ""
// and the key has no versions, check is called with the zero ObjectInfo and
// found = false (found = true otherwise) and, if it returns nil, the delete
// happens with the semantics of Backend.DeleteObject: a delete marker in a
// versioned (Enabled or Suspended) bucket, nothing in an unversioned one.
// The S3 layer uses it for a delete authorized on the observation that the
// key had no object: the delete marker is still created, but not when an
// object appeared in the meantime. A missing version (versionID != "")
// yields ErrVersionNotFound without calling check. The rules for check are
// those of DeleteObjectIf.
type CheckedDeleter interface {
	DeleteObjectChecked(ctx context.Context, bucket, key, versionID string, check func(info ObjectInfo, found bool) error) (DeleteResult, error)
}

// ConditionalMetaUpdater makes a metadata update conditional, atomically.
//
// UpdateObjectMetaIf is Backend.UpdateObjectMeta with a precondition: check
// is called with the addressed version (versionID "" = the latest) as it is
// stored when the update is applied, and the update happens only when check
// returns nil; check's error is returned unchanged otherwise. The S3 layer
// uses it to apply an ACL, tags, retention or a legal hold to exactly the
// object version a request was authorized for, not to an object that
// replaced it in the meantime (in unversioned and versioning-suspended
// buckets every overwrite reuses the version ID "null"). Missing keys,
// versions and delete markers fail as for UpdateObjectMeta, without calling
// check.
//
// check must not do I/O or call into the backend, and it may be called
// more than once (the last call decides).
type ConditionalMetaUpdater interface {
	UpdateObjectMetaIf(ctx context.Context, bucket, key, versionID string, check func(ObjectInfo) error, fn func(*ObjectMeta) error) (ObjectInfo, error)
}

// ConfigViewer gives read access to a stored bucket configuration document
// without copying it.
//
// ViewBucketConfig calls fn with the document stored under key (see the
// Config* constants), with the errors of Backend.GetBucketConfig otherwise
// (ErrConfigNotFound, ErrBucketNotFound), and returns fn's error. data is
// only valid during the call: fn must not retain or modify it, and must
// not call into the backend. It lets callers that only need to recognise a
// document they have already parsed (a digest or a comparison against a
// cached copy) avoid allocating a copy of a large document per request.
type ConfigViewer interface {
	ViewBucketConfig(ctx context.Context, bucket, key string, fn func(data []byte) error) error
}

// UploadPosition is a position in the order in which multipart uploads are
// listed: by key, then Initiated (oldest first), then UploadID. The zero
// value is the start of the listing.
type UploadPosition struct {
	Key       string
	Initiated time.Time
	UploadID  string
	// AfterKey places the position after every upload of Key; Initiated
	// and UploadID are then ignored.
	AfterKey bool
}

// PositionOf returns the position of upload u.
func PositionOf(u UploadInfo) UploadPosition {
	return UploadPosition{Key: u.Key, Initiated: u.Initiated, UploadID: u.UploadID}
}

// Precedes reports whether p is strictly before upload u in listing order,
// i.e. whether a walk starting after p includes u.
func (p UploadPosition) Precedes(u UploadInfo) bool {
	if p == (UploadPosition{}) {
		return true
	}
	if u.Key != p.Key {
		return u.Key > p.Key
	}
	if p.AfterKey {
		return false
	}
	if c := u.Initiated.Compare(p.Initiated); c != 0 {
		return c > 0
	}
	return u.UploadID > p.UploadID
}

// UploadWalker walks in-progress multipart uploads in listing order
// without loading all of a bucket's uploads at once, so that a page of a
// listing costs time proportional to the page, not to the number of
// pending uploads.
type UploadWalker interface {
	// WalkMultipartUploads calls fn for each in-progress upload of bucket
	// whose key has the given prefix and that sorts strictly after the
	// position after, in the order of Backend.ListMultipartUploads.
	// Returning ErrStopWalk from fn stops the walk (WalkMultipartUploads
	// then returns nil); any other error is returned as-is. fn runs with
	// no lock or transaction held and may call into the backend. Like the
	// object walks, the walk is not a snapshot when uploads change
	// concurrently.
	WalkMultipartUploads(ctx context.Context, bucket, prefix string, after UploadPosition, fn func(UploadInfo) error) error
}

// CompressionDeflate is the compression algorithm of the built-in
// backends: DEFLATE in independently compressed blocks (package
// internal/storage/compress), so that reads can seek without inflating
// everything before the requested range.
const CompressionDeflate = "deflate"

// Compression asks a CompressingBackend to store an object's data
// compressed. The zero value means no compression.
type Compression struct {
	Algorithm string // CompressionDeflate, or "" for none
	Level     int    // algorithm level (DEFLATE: 1 fastest to 9 smallest)
}

// Enabled reports whether c asks for compression.
func (c Compression) Enabled() bool { return c.Algorithm != "" }

// WriteOptions are backend-level options for writing an object version.
type WriteOptions struct {
	Compression Compression
}

// CompressingBackend stores object data compressed on request.
// Compression is transparent: ObjectInfo.Size, ETags, checksums and the
// content returned by OpenObject are those of the uncompressed data, and
// OpenObject's reader seeks in uncompressed offsets. ObjectInfo.Compression
// and StoredSize report how a version is stored. Versions are read the
// same way whether or not they were stored compressed, so the setting can
// change at any time.
//
// The methods are Backend.NewObjectWriter and
// Backend.CompleteMultipartUpload with write options. A backend may store
// a version uncompressed although compression was requested (e.g. an
// empty object); it reports that in ObjectInfo.Compression. It returns an
// error for an algorithm or level it does not support.
type CompressingBackend interface {
	NewObjectWriterWith(ctx context.Context, bucket, key string, opts WriteOptions) (ObjectWriter, error)
	CompleteMultipartUploadWith(ctx context.Context, bucket, key, uploadID string, partNumbers []int,
		meta ObjectMeta, opts CommitOptions, wopts WriteOptions) (ObjectInfo, error)
}
