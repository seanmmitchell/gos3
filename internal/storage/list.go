package storage

import (
	"context"
	"strings"
)

// Generic listing helpers built on Backend.WalkObjects / WalkVersions.
// They implement S3 prefix/delimiter grouping, max-keys truncation and markers,
// so every backend gets correct ListObjects(V1/V2)/ListObjectVersions for free.
//
// CONTRACT: signatures are fixed; the implementation is owned by the
// storage-memory work item (see docs/ARCHITECTURE.md).
//
// Common prefixes: with a non-empty Delimiter, a key K under Prefix whose
// remainder K[len(Prefix):] contains Delimiter is rolled up into the common
// prefix Prefix + remainder[:index(Delimiter)+len(Delimiter)]. Each distinct
// common prefix is returned once and counts as one entry toward MaxKeys.
// After emitting a common prefix p the walk restarts strictly after
// p + "\xff" (valid UTF-8 never contains 0xFF), so all keys under p are
// skipped with one O(log n) seek instead of being enumerated.

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

// DefaultMaxKeys is used when ListParams.MaxKeys / VersionListParams.MaxKeys
// is <= 0.
const DefaultMaxKeys = 1000

// skipSuffix is appended to a common prefix to form a walk marker that sorts
// after every UTF-8 key starting with that prefix.
const skipSuffix = "\xff"

// CommonPrefix returns the common prefix key rolls up into for the given
// listing prefix and delimiter, and whether it rolls up at all (it does not
// when delimiter is empty, key lacks prefix, or the remainder after prefix
// does not contain delimiter).
func CommonPrefix(key, prefix, delimiter string) (string, bool) {
	if delimiter == "" || !strings.HasPrefix(key, prefix) {
		return "", false
	}
	i := strings.Index(key[len(prefix):], delimiter)
	if i < 0 {
		return "", false
	}
	return key[:len(prefix)+i+len(delimiter)], true
}

// startMarker translates a listing marker into a walk marker: when the
// marker is itself a common prefix (as returned as NextAfter /
// NextKeyMarker on a page that ended with a common prefix) everything under
// it is skipped, matching S3.
func startMarker(marker, prefix, delimiter string) (walkMarker string, skippedPrefix string) {
	if cp, ok := CommonPrefix(marker, prefix, delimiter); ok && cp == marker {
		return marker + skipSuffix, marker
	}
	return marker, ""
}

func maxKeys(n int) int {
	if n <= 0 {
		return DefaultMaxKeys
	}
	return n
}

// ListObjects lists the latest versions of objects in a bucket with S3
// delimiter semantics. Each common prefix counts as one key toward MaxKeys.
//
// Keys and common prefixes strictly greater than p.After are returned; if
// p.After is itself a common prefix of this listing, everything under it is
// skipped too. IsTruncated is true only when at least one more key or common
// prefix exists; NextAfter is set only then (to the last key or common
// prefix returned).
func ListObjects(ctx context.Context, b Backend, bucket string, p ListParams) (ListResult, error) {
	limit := maxKeys(p.MaxKeys)
	var res ListResult
	after, lastCP := startMarker(p.After, p.Prefix, p.Delimiter)
	count := 0
	for {
		restart := false
		err := b.WalkObjects(ctx, bucket, p.Prefix, after, func(o ObjectInfo) error {
			if lastCP != "" && strings.HasPrefix(o.Key, lastCP) {
				// Only reachable for keys with invalid UTF-8 sorting after
				// lastCP+"\xff"; they belong to an already emitted prefix.
				return nil
			}
			if count == limit {
				res.IsTruncated = true
				return ErrStopWalk
			}
			count++
			if cp, ok := CommonPrefix(o.Key, p.Prefix, p.Delimiter); ok {
				res.CommonPrefixes = append(res.CommonPrefixes, cp)
				res.NextAfter = cp
				lastCP = cp
				after = cp + skipSuffix
				restart = true
				return ErrStopWalk
			}
			res.Objects = append(res.Objects, o)
			res.NextAfter = o.Key
			return nil
		})
		if err != nil {
			return ListResult{}, err
		}
		if !restart || res.IsTruncated {
			break
		}
		if err := ctx.Err(); err != nil {
			return ListResult{}, err
		}
	}
	if !res.IsTruncated {
		res.NextAfter = ""
	}
	return res, nil
}

// ListObjectVersions lists versions and delete markers with S3 delimiter
// semantics. Each version, delete marker and common prefix counts as one
// entry toward MaxKeys.
//
// Markers follow S3: KeyMarker alone starts after all versions of that key;
// KeyMarker + VersionIDMarker starts after that version; a KeyMarker equal
// to a common prefix of this listing skips everything under it (the
// VersionIDMarker is then ignored). When truncated, NextKeyMarker /
// NextVersionIDMarker identify the last entry returned; if that entry is a
// common prefix, NextKeyMarker is the prefix and NextVersionIDMarker is "".
// Both are "" when not truncated.
func ListObjectVersions(ctx context.Context, b Backend, bucket string, p VersionListParams) (VersionListResult, error) {
	limit := maxKeys(p.MaxKeys)
	var res VersionListResult
	keyMarker, vidMarker := p.KeyMarker, p.VersionIDMarker
	if keyMarker == "" {
		vidMarker = ""
	}
	var lastCP string
	if km, skipped := startMarker(keyMarker, p.Prefix, p.Delimiter); skipped != "" {
		keyMarker, vidMarker, lastCP = km, "", skipped
	}
	count := 0
	for {
		restart := false
		err := b.WalkVersions(ctx, bucket, p.Prefix, keyMarker, vidMarker, func(v ObjectInfo) error {
			if lastCP != "" && strings.HasPrefix(v.Key, lastCP) {
				return nil
			}
			if count == limit {
				res.IsTruncated = true
				return ErrStopWalk
			}
			count++
			if cp, ok := CommonPrefix(v.Key, p.Prefix, p.Delimiter); ok {
				res.CommonPrefixes = append(res.CommonPrefixes, cp)
				res.NextKeyMarker, res.NextVersionIDMarker = cp, ""
				lastCP = cp
				keyMarker, vidMarker = cp+skipSuffix, ""
				restart = true
				return ErrStopWalk
			}
			res.Versions = append(res.Versions, v)
			res.NextKeyMarker, res.NextVersionIDMarker = v.Key, v.VersionID
			return nil
		})
		if err != nil {
			return VersionListResult{}, err
		}
		if !restart || res.IsTruncated {
			break
		}
		if err := ctx.Err(); err != nil {
			return VersionListResult{}, err
		}
	}
	if !res.IsTruncated {
		res.NextKeyMarker, res.NextVersionIDMarker = "", ""
	}
	return res, nil
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

// ComputeBucketUsage walks a bucket to compute usage statistics. It is
// O(number of versions); callers should cache the result for large buckets.
func ComputeBucketUsage(ctx context.Context, b Backend, bucket string) (BucketUsage, error) {
	var u BucketUsage
	err := b.WalkVersions(ctx, bucket, "", "", "", func(v ObjectInfo) error {
		if v.DeleteMarker {
			u.DeleteMarkers++
			return nil
		}
		u.Versions++
		u.VersionBytes += v.Size
		if v.IsLatest {
			u.Objects++
			u.Bytes += v.Size
		}
		return nil
	})
	if err != nil {
		return BucketUsage{}, err
	}
	uploads, err := b.ListMultipartUploads(ctx, bucket)
	if err != nil {
		return BucketUsage{}, err
	}
	u.PendingUploads = int64(len(uploads))
	return u, nil
}
