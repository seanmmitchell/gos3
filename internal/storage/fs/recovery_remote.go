package fs

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// remoteRecoveryStats counts what the recovery pass did on one remote.
type remoteRecoveryStats struct {
	dataBlobs int // unreferenced data blobs deleted
	partBlobs int // unreferenced part blobs deleted
	young     int // unreferenced blobs kept because they are younger than the retention
	ignored   int // keys with names gos3 does not generate (left alone)
	errors    int
}

// remoteCandidate is a listed blob that may be an orphan.
type remoteCandidate struct {
	key            string
	bucketOrUpload string // bucket ID (data) or upload ID (parts)
	name           string // data ID or part file name
}

// recoverRemotes runs the recovery pass on every configured remote. A blob
// is deleted only when its name is one gos3 generates, it is not pending
// (a write in flight) or held by an open reader, the metadata does not
// reference it, and its remote LastModified is older than the retention
// period: the age threshold takes the place of the local quarantine, so
// data written after a backup of meta.db survives restoring that backup
// for the retention period.
func (b *Backend) recoverRemotes(ctx context.Context, started time.Time) {
	for _, r := range b.sortedRemotes() {
		if ctx.Err() != nil {
			return
		}
		var st remoteRecoveryStats
		begin := time.Now()
		cutoff := started.Add(-b.retention)
		b.recoverRemoteArea(ctx, r, r.dataPrefix(), cutoff, false, &st)
		b.recoverRemoteArea(ctx, r, r.uploadPrefix(), cutoff, true, &st)
		if ctx.Err() != nil {
			b.log.Debug("fs: remote recovery interrupted by close", "backing", r.name)
			return
		}
		attrs := []any{
			"backing", r.name,
			"bucket", r.bucket,
			"prefix", r.prefix,
			"orphan_data_blobs", st.dataBlobs,
			"orphan_part_blobs", st.partBlobs,
			"kept_young_blobs", st.young,
			"unrecognized_keys", st.ignored,
			"errors", st.errors,
			"retention", b.retention,
			"duration", time.Since(begin).Round(time.Millisecond),
		}
		switch {
		case st.dataBlobs+st.partBlobs > 0:
			b.log.Warn("fs: recovery deleted unreferenced remote blobs older than the retention period", attrs...)
		case st.young+st.errors > 0:
			b.log.Info("fs: remote recovery finished", attrs...)
		default:
			b.log.Debug("fs: remote recovery found nothing to clean", attrs...)
		}
	}
}

// recoverRemoteArea lists the blobs below prefix (data/ or uploads/) page
// by page and deletes the orphans among them.
func (b *Backend) recoverRemoteArea(ctx context.Context, r *remote, prefix string, cutoff time.Time, parts bool, st *remoteRecoveryStats) {
	in := s3client.ListInput{Prefix: prefix}
	for {
		if ctx.Err() != nil {
			return
		}
		page, err := r.client.ListObjectsV2(ctx, r.bucket, in)
		if err != nil {
			if ctx.Err() == nil {
				st.errors++
				b.log.Warn("fs: recovery: list remote blobs", "backing", r.name, "prefix", prefix, "err", err)
			}
			return
		}
		if err := b.recoverRemotePage(ctx, r, prefix, page.Objects, cutoff, parts, st); err != nil {
			if ctx.Err() == nil && !errors.Is(err, ErrClosed) {
				st.errors++
				b.log.Warn("fs: recovery: check remote blobs", "backing", r.name, "err", err)
			}
			return
		}
		if !page.IsTruncated {
			return
		}
		in.ContinuationToken = page.NextContinuationToken
	}
}

// parseRemoteKey splits rest (a key below data/ or uploads/) into its two
// components if gos3 could have generated it.
func parseRemoteKey(rest string, parts bool) (first, second string, ok bool) {
	first, second, ok = strings.Cut(rest, "/")
	if !ok {
		return "", "", false
	}
	if parts {
		if !validUploadID(first) {
			return "", "", false
		}
		_, ok = parsePartFileName(second)
		return first, second, ok
	}
	return first, second, validBucketID(first) && validDataID(second)
}

func (b *Backend) recoverRemotePage(ctx context.Context, r *remote, prefix string, objs []s3client.ObjectAttrs, cutoff time.Time, parts bool, st *remoteRecoveryStats) error {
	// Pending and reader checks strictly before the metadata check.
	var candidates []remoteCandidate
	for _, o := range objs {
		rest, _ := trimKey(o.Key, prefix)
		first, second, ok := parseRemoteKey(rest, parts)
		if !ok {
			st.ignored++
			b.log.Debug("fs: recovery: leaving unrecognized remote key alone", "backing", r.name, "key", o.Key)
			continue
		}
		id := r.blobID(o.Key)
		if b.pending.has(id) || b.blobs.held(id) {
			continue
		}
		// The age is checked after the metadata (below), so that only
		// actual orphans are counted as kept young.
		candidates = append(candidates, remoteCandidate{key: o.Key, bucketOrUpload: first, name: second})
	}
	if len(candidates) == 0 {
		return nil
	}
	ages := make(map[string]time.Time, len(objs))
	for _, o := range objs {
		ages[o.Key] = o.LastModified
	}
	var orphans []remoteCandidate
	if err := b.view(func(tx *bbolt.Tx) error {
		orphans = orphans[:0]
		for _, c := range candidates {
			if !remoteReferenced(tx, c, parts) {
				orphans = append(orphans, c)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, c := range orphans {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if lm := ages[c.key]; lm.IsZero() || !lm.Before(cutoff) {
			st.young++
			continue
		}
		// A reader may have opened the blob since the check above only if
		// it is referenced, which it is not; deleteOrDefer keeps the
		// registry consistent nevertheless.
		if !b.blobs.deleteOrDefer(r.blobID(c.key)) {
			continue
		}
		if err := r.client.DeleteObject(ctx, r.bucket, c.key); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			st.errors++
			b.log.Warn("fs: recovery: delete remote blob", "backing", r.name, "key", c.key, "err", err)
			continue
		}
		if parts {
			st.partBlobs++
		} else {
			st.dataBlobs++
		}
	}
	return nil
}

// remoteReferenced reports whether the metadata references a remote blob.
func remoteReferenced(tx *bbolt.Tx, c remoteCandidate, parts bool) bool {
	if !parts {
		bb := tx.Bucket(bucketDataName(c.bucketOrUpload))
		return bb != nil && bb.Bucket(subData).Get([]byte(c.name)) != nil
	}
	v := tx.Bucket(topUploads).Get([]byte(c.bucketOrUpload))
	if v == nil {
		return false
	}
	bb := tx.Bucket(bucketDataName(string(v)))
	if bb == nil {
		return false
	}
	n, _ := parsePartFileName(c.name)
	raw := bb.Bucket(subParts).Get(partKey(c.bucketOrUpload, n))
	if raw == nil {
		return false
	}
	var rec partRecord
	if err := decodeRecord(raw, &rec); err != nil {
		return true // never delete what cannot be checked
	}
	return rec.File == c.name
}
