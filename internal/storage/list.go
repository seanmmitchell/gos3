package storage

import (
	"context"
	"errors"
)

// Generic listing helpers built on Backend.WalkObjects / WalkVersions.
// They implement S3 prefix/delimiter grouping, max-keys truncation and markers,
// so every backend gets correct ListObjects(V1/V2)/ListObjectVersions for free.
//
// CONTRACT STUB: signatures are fixed; the implementation is owned by the
// storage-memory work item (see docs/ARCHITECTURE.md).

// ListParams controls ListObjects.
type ListParams struct {
	Prefix    string
	Delimiter string
	// After: return keys (and common prefixes) strictly greater than this
	// (V1 Marker, V2 StartAfter or decoded ContinuationToken).
	After   string
	MaxKeys int // <= 0 means 1000; values > 1000 are clamped to 1000 by the S3 layer
}

// ListResult is the result of ListObjects.
type ListResult struct {
	Objects        []ObjectInfo
	CommonPrefixes []string
	IsTruncated    bool
	// NextAfter is the last key or common prefix returned (use as the next
	// After / NextMarker / continuation) when IsTruncated.
	NextAfter string
}

// VersionListParams controls ListObjectVersions.
type VersionListParams struct {
	Prefix          string
	Delimiter       string
	KeyMarker       string
	VersionIDMarker string
	MaxKeys         int
}

// VersionListResult is the result of ListObjectVersions. Versions contains
// both object versions and delete markers (DeleteMarker=true), in order.
type VersionListResult struct {
	Versions            []ObjectInfo
	CommonPrefixes      []string
	IsTruncated         bool
	NextKeyMarker       string
	NextVersionIDMarker string
}

var errUnimplemented = errors.New("storage: list helpers not implemented yet")

// ListObjects lists the latest versions of objects in a bucket with S3
// delimiter semantics. Each common prefix counts as one key toward MaxKeys.
func ListObjects(ctx context.Context, b Backend, bucket string, p ListParams) (ListResult, error) {
	return ListResult{}, errUnimplemented
}

// ListObjectVersions lists versions and delete markers with S3 delimiter semantics.
func ListObjectVersions(ctx context.Context, b Backend, bucket string, p VersionListParams) (VersionListResult, error) {
	return VersionListResult{}, errUnimplemented
}

// BucketUsage returns the number of current objects and their total size
// (latest versions only) and, separately, the total count/size of all versions.
type BucketUsage struct {
	Objects        int64
	Bytes          int64
	Versions       int64 // all versions incl. noncurrent (excluding delete markers)
	VersionBytes   int64
	DeleteMarkers  int64
	PendingUploads int64
}

// ComputeBucketUsage walks a bucket to compute usage statistics.
func ComputeBucketUsage(ctx context.Context, b Backend, bucket string) (BucketUsage, error) {
	return BucketUsage{}, errUnimplemented
}
