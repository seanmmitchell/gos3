package storage

import (
	"context"
	"errors"
	"strings"
)

// Listing helpers for in-progress multipart uploads. They use the backend's
// UploadWalker when it has one, so a page costs time proportional to its
// size rather than to the number of pending uploads of the bucket, and fall
// back to filtering Backend.ListMultipartUploads otherwise.

// WalkMultipartUploads calls fn for each in-progress upload of bucket whose
// key has the given prefix and that sorts strictly after the position after,
// in listing order (see UploadWalker). Returning ErrStopWalk from fn stops
// the walk and WalkMultipartUploads returns nil.
func WalkMultipartUploads(ctx context.Context, b Backend, bucket, prefix string, after UploadPosition, fn func(UploadInfo) error) error {
	if w, ok := b.(UploadWalker); ok {
		return w.WalkMultipartUploads(ctx, bucket, prefix, after, fn)
	}
	all, err := b.ListMultipartUploads(ctx, bucket)
	if err != nil {
		return err
	}
	for _, u := range all {
		if !strings.HasPrefix(u.Key, prefix) || !after.Precedes(u) {
			continue
		}
		if err := fn(u); err != nil {
			if errors.Is(err, ErrStopWalk) {
				return nil
			}
			return err
		}
	}
	return nil
}

// UploadListParams controls ListUploads (the ListMultipartUploads query
// parameters).
type UploadListParams struct {
	Prefix         string
	Delimiter      string
	KeyMarker      string
	UploadIDMarker string
	// MaxUploads is the maximum number of uploads and common prefixes
	// returned; <= 0 returns an empty, untruncated page.
	MaxUploads int
}

// UploadListResult is a page of ListUploads.
type UploadListResult struct {
	// Uploads are the listed uploads, their Meta trimmed to what a listing
	// reports (see ListingMeta).
	Uploads        []UploadInfo
	CommonPrefixes []string
	IsTruncated    bool
	// NextKeyMarker / NextUploadIDMarker identify the last entry returned
	// when IsTruncated (a common prefix has an empty upload ID marker);
	// both are "" otherwise.
	NextKeyMarker      string
	NextUploadIDMarker string
}

// ListUploads lists a page of in-progress multipart uploads with S3
// ListMultipartUploads semantics:
//
//   - only keys with the prefix are listed; with a delimiter, keys whose
//     remainder after the prefix contains it roll up into one common prefix
//     each, counting as one entry toward MaxUploads;
//   - KeyMarker alone starts after every upload of that key; with
//     UploadIDMarker the uploads of KeyMarker following that upload are
//     included too (those with a greater upload ID when the marker upload
//     no longer exists); UploadIDMarker without KeyMarker is ignored; a
//     KeyMarker equal to one of the listing's common prefixes skips
//     everything under it;
//   - IsTruncated is true only when at least one more entry exists.
func ListUploads(ctx context.Context, b Backend, bucket string, p UploadListParams) (UploadListResult, error) {
	var res UploadListResult
	if p.MaxUploads <= 0 {
		return res, nil
	}
	var (
		pos    UploadPosition
		lastCP string // uploads under this common prefix are skipped
		skipID string // marker upload gone: skip KeyMarker uploads with IDs <= skipID
	)
	if p.KeyMarker != "" {
		if km, skipped := startMarker(p.KeyMarker, p.Prefix, p.Delimiter); skipped != "" {
			pos, lastCP = UploadPosition{Key: km, AfterKey: true}, skipped
		} else if p.UploadIDMarker == "" {
			pos = UploadPosition{Key: p.KeyMarker, AfterKey: true}
		} else {
			u, err := b.GetMultipartUpload(ctx, bucket, p.KeyMarker, p.UploadIDMarker)
			switch {
			case err == nil:
				pos = PositionOf(u)
			case errors.Is(err, ErrUploadNotFound):
				pos, skipID = UploadPosition{Key: p.KeyMarker}, p.UploadIDMarker
			default:
				return UploadListResult{}, err
			}
		}
	}
	count := 0
	for {
		restart := false
		err := WalkMultipartUploads(ctx, b, bucket, p.Prefix, pos, func(u UploadInfo) error {
			if lastCP != "" && strings.HasPrefix(u.Key, lastCP) {
				return nil
			}
			if skipID != "" && u.Key == p.KeyMarker && u.UploadID <= skipID {
				return nil
			}
			if count == p.MaxUploads {
				res.IsTruncated = true
				return ErrStopWalk
			}
			count++
			if cp, ok := CommonPrefix(u.Key, p.Prefix, p.Delimiter); ok {
				res.CommonPrefixes = append(res.CommonPrefixes, cp)
				res.NextKeyMarker, res.NextUploadIDMarker = cp, ""
				lastCP = cp
				pos = UploadPosition{Key: cp + skipSuffix, AfterKey: true}
				restart = true
				return ErrStopWalk
			}
			u.Meta = ListingMeta(u.Meta)
			res.Uploads = append(res.Uploads, u)
			res.NextKeyMarker, res.NextUploadIDMarker = u.Key, u.UploadID
			return nil
		})
		if err != nil {
			return UploadListResult{}, err
		}
		if !restart || res.IsTruncated {
			break
		}
		if err := ctx.Err(); err != nil {
			return UploadListResult{}, err
		}
	}
	if !res.IsTruncated {
		res.NextKeyMarker, res.NextUploadIDMarker = "", ""
	}
	return res, nil
}

// CountMultipartUploads returns the number of in-progress uploads of bucket
// without holding them all in memory.
func CountMultipartUploads(ctx context.Context, b Backend, bucket string) (int64, error) {
	var n int64
	err := WalkMultipartUploads(ctx, b, bucket, "", UploadPosition{}, func(UploadInfo) error {
		n++
		return nil
	})
	return n, err
}
