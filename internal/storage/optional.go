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
