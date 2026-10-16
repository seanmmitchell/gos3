package storage

import (
	"context"
	"io"
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

// NewerVersionsDeleter makes the permanent delete of a version conditional
// on the version and on the versions newer than it, atomically.
//
// DeleteVersionIf is DeleteObjectIf for a version ID (versionID must not be
// ""), except that check also receives the versions of key that are newer
// than the addressed one, nearest first, at most limit of them (limit < 1
// is taken as 1): newer[0] is the version whose creation made the
// addressed one noncurrent, and when len(newer) < limit the last entry is
// the key's latest version (with IsLatest set). newer is empty when the
// addressed version is the latest. The lifecycle worker uses it to apply
// NoncurrentDays and NewerNoncurrentVersions to a key's versions as they
// are at the delete, not as an earlier walk saw them (a newer version
// removed meanwhile must stop the delete).
//
// check must not do I/O, call into the backend or retain newer, and it may
// be called more than once (the last call decides).
type NewerVersionsDeleter interface {
	DeleteVersionIf(ctx context.Context, bucket, key, versionID string, limit int, check func(v ObjectInfo, newer []ObjectInfo) error) (DeleteResult, error)
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

// VersionImporter stores object versions under the identity they already
// have on another backend. It is what makes replication and moving buckets
// between backings (internal/storage/multi) possible: a copy must keep the
// version IDs clients know, the LastModified times and the order of a
// key's versions.
//
// Imported version IDs come from another backend and need not have the
// shape of the IDs this backend generates; every method that takes a
// version ID (GetObjectInfo, OpenObject, UpdateObjectMeta, DeleteObject,
// the version-ID marker of WalkVersions, the optional extensions) must find
// imported versions too.
type VersionImporter interface {
	// NewVersionWriter starts writing the data of an imported version of
	// bucket/key. ErrBucketNotFound if the bucket does not exist;
	// ErrInvalidName for keys the backend cannot store.
	NewVersionWriter(ctx context.Context, bucket, key string) (VersionWriter, error)
}

// VersionWriter streams the data of an imported version. Nothing becomes
// visible until Commit succeeds. Exactly one of Commit or Abort should be
// called; Abort after Commit is a no-op.
type VersionWriter interface {
	io.Writer
	// Commit atomically stores the written bytes as the newest version of
	// the key, whatever the bucket's versioning status, with exactly
	// info.VersionID, info.LastModified, info.DeleteMarker and (for objects)
	// info.ObjectMeta. info.Bucket, Key, Size and IsLatest are ignored: the
	// size is that of the written data. An existing version with the same
	// version ID (for NullVersionID: the key's null version or null delete
	// marker) is replaced, wherever it is in the key's history. A delete
	// marker must have no data (otherwise ErrInvalidName). info.VersionID
	// must be NullVersionID or 1-64 URL-safe characters ([A-Za-z0-9_.-]),
	// otherwise ErrInvalidName. Returns the stored info (IsLatest true).
	Commit(ctx context.Context, info ObjectInfo) (ObjectInfo, error)
	Abort() error
}

// ValidImportedVersionID reports whether id may be passed to
// VersionWriter.Commit: NullVersionID or 1-64 URL-safe characters.
func ValidImportedVersionID(id string) bool {
	if id == NullVersionID {
		return true
	}
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
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
	Encryption  string // EncryptionAES256, EncryptionRestic, or "" for none
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
// Encryption uses the same write-options methods: an unsupported encryption
// provider must fail, never fall back to plaintext. ObjectMeta.Encryption in
// the committed result describes the actual data encoding.
type CompressingBackend interface {
	NewObjectWriterWith(ctx context.Context, bucket, key string, opts WriteOptions) (ObjectWriter, error)
	CompleteMultipartUploadWith(ctx context.Context, bucket, key, uploadID string, partNumbers []int,
		meta ObjectMeta, opts CommitOptions, wopts WriteOptions) (ObjectInfo, error)
}

// CompressingImporter is a VersionImporter that can store an imported
// version compressed (see CompressingBackend), so that a copy takes the
// same space as its source. NewVersionWriterWith is
// VersionImporter.NewVersionWriter with write options; as with
// CompressingBackend, the backend may store the data uncompressed anyway
// and reports how it stored it in the info Commit returns.
type CompressingImporter interface {
	NewVersionWriterWith(ctx context.Context, bucket, key string, opts WriteOptions) (VersionWriter, error)
}
