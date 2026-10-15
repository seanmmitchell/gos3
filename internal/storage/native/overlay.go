package native

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Overlay records
//
// Metadata gos3 keeps for an object version itself is held in overlay
// records in the StateStore (record key m/<bucket>/<b64(key)>/<b64(version)>/,
// see state.go): a metaPatch (meta.go) plus the identity of the remote
// object version it was made for, its remote ETag and, for "null"
// versions, its remote LastModified. They hold:
//
//   - the ACL and the website redirect, always (never the remote object's
//     headers, which anybody who can write the remote bucket could set);
//   - metadata changed after the object was written (UpdateObjectMeta
//     never rewrites an object);
//   - metadata known only after the remote write (a remote multipart
//     completion's ETag, checksums and part sizes) or that did not fit
//     into the object's headers.
//
// All overlays are loaded at Open and written through, always after the
// remote acknowledged the write they describe (a failed write leaves
// nothing behind; a failed overlay write fails the operation, leaving the
// object private, the safe default).
//
// An overlay applies only to the object version it was made for: the
// version ID and the remote ETag must match and, for "null" versions
// (which every overwrite of an unversioned or versioning-suspended bucket
// reuses), the LastModified second too. When another S3 client replaces
// the object, or restores or copies an older version, its overlay (say, a
// public-read ACL) is therefore not inherited by the new object; a
// replacement within the same second by identical content is
// indistinguishable and keeps it. gos3's own writes and deletes of a null
// version hide its overlay while the remote request is in flight and drop
// it once the remote acknowledged the request (restoring it otherwise).
//
// Overlays of versions removed outside gos3 are pruned when a lookup by
// version ID finds the version gone, when a version listing of the key
// proves it gone, and by a background sweep (each bucket at most once a
// day, listing the versions of the keys that have overlays). Only
// overlays stored before the lookup or listing started are pruned, so an
// overlay of a version created meanwhile is never mistaken for a stale
// one.
//
// Listings apply overlays too, so a listing reports the ETag and ACL that
// HEAD reports.

// overlayID names the object version an overlay belongs to.
type overlayID struct {
	bucket, key, vid string
}

// bucketKey names an object key of a bucket (the overlays are indexed by
// key, then version ID).
type bucketKey struct {
	bucket, key string
}

// overlay is an overlay record. It is immutable once stored in
// Store.overlays (updates store a new one).
type overlay struct {
	// RemoteETag is the remote ETag of the version.
	RemoteETag string `json:"re"`
	// LastModified is the remote LastModified of the version (Unix
	// seconds); it identifies "null" versions together with the ETag.
	LastModified int64 `json:"lm,omitempty"`
	// Meta holds the metadata that replaces the remote object's.
	Meta metaPatch `json:"m"`

	// at is when the overlay was stored in memory (zero for overlays
	// loaded at Open): pruning only drops overlays older than the lookup
	// that found their version gone.
	at time.Time
}

// matches reports whether o was made for the remote version whose ETag
// and LastModified are given (vid is the gos3 version ID).
func (o *overlay) matches(vid, etag string, lastModified time.Time) bool {
	if o == nil || o.RemoteETag != etag {
		return false
	}
	return vid != storage.NullVersionID || o.LastModified == truncTime(lastModified).Unix()
}

// getOverlayLocked returns the overlay of id (s.mu held).
func (s *Store) getOverlayLocked(id overlayID) *overlay {
	return s.overlays[bucketKey{id.bucket, id.key}][id.vid]
}

// setOverlayLocked stores (o != nil) or removes the overlay of id (s.mu
// held).
func (s *Store) setOverlayLocked(id overlayID, o *overlay) {
	bk := bucketKey{id.bucket, id.key}
	vs := s.overlays[bk]
	if o == nil {
		delete(vs, id.vid)
		if len(vs) == 0 {
			delete(s.overlays, bk)
		}
		return
	}
	if vs == nil {
		vs = map[string]*overlay{}
		s.overlays[bk] = vs
	}
	vs[id.vid] = o
}

// overlayFor returns the overlay of a version if it applies to the remote
// object with the given ETag and LastModified (nil otherwise).
func (s *Store) overlayFor(bucket, key, vid, etag string, lastModified time.Time) *overlay {
	s.mu.Lock()
	o := s.getOverlayLocked(overlayID{bucket, key, vid})
	s.mu.Unlock()
	if !o.matches(vid, etag, lastModified) {
		return nil
	}
	return o
}

// hasOverlays reports whether bucket/key has overlays.
func (s *Store) hasOverlays(bucket, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.overlays[bucketKey{bucket, key}]) > 0
}

// putOverlay merges patch into the overlay of a version (replacing an
// overlay made for another remote object) and stores it. The caller holds
// the object's lock. On failure the in-memory state is unchanged; the
// record may have been written anyway (see StateStore.PutRecord).
func (s *Store) putOverlay(ctx context.Context, id overlayID, etag string, lastModified time.Time, patch *metaPatch) error {
	if patch.empty() {
		return nil
	}
	s.mu.Lock()
	cur := s.getOverlayLocked(id)
	s.mu.Unlock()
	o := &overlay{RemoteETag: etag, LastModified: truncTime(lastModified).Unix()}
	if cur.matches(id.vid, etag, lastModified) {
		o.Meta = cur.Meta
	}
	o.Meta = o.Meta.merge(patch)
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if err := s.state.PutRecord(ctx, overlayKey(id), data); err != nil {
		return fmt.Errorf("native %q: store the metadata of %s/%s version %q: %w", s.name, id.bucket, id.key, id.vid, err)
	}
	o.at = time.Now()
	s.mu.Lock()
	s.setOverlayLocked(id, o)
	s.mu.Unlock()
	return nil
}

// hideOverlay removes the overlay of id from memory, so readers stop
// using it, and returns it (nil if there is none). The caller holds the
// object's lock and then either restores it (restoreOverlay: the remote
// request failed) or forgets it (forgetOverlay: the remote acknowledged
// the request that replaced or removed the version).
func (s *Store) hideOverlay(id overlayID) *overlay {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.getOverlayLocked(id)
	if o != nil {
		s.setOverlayLocked(id, nil)
	}
	return o
}

// restoreOverlay undoes hideOverlay.
func (s *Store) restoreOverlay(id overlayID, o *overlay) {
	if o == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getOverlayLocked(id) == nil {
		s.setOverlayLocked(id, o)
	}
}

// forgetOverlay deletes the record of an overlay hidden by hideOverlay. A
// record that cannot be deleted is logged: after a restart it applies only
// to the remote object it was made for, which is gone.
func (s *Store) forgetOverlay(ctx context.Context, id overlayID, o *overlay) {
	if o == nil {
		return
	}
	if err := s.state.DeleteRecords(context.WithoutCancel(ctx), overlayKey(id)); err != nil {
		s.log.Warn("native: delete overlay record", "backing", s.name, "bucket", id.bucket, "key", id.key, "version", id.vid, "err", err)
	}
}

// dropOverlay forgets the overlay of a version that gos3 deleted. The
// caller holds the object's lock.
func (s *Store) dropOverlay(ctx context.Context, id overlayID) {
	s.forgetOverlay(ctx, id, s.hideOverlay(id))
}

// dropBucketOverlays forgets every overlay of bucket.
func (s *Store) dropBucketOverlays(ctx context.Context, bucket string) error {
	s.mu.Lock()
	n := len(s.overlays)
	maps.DeleteFunc(s.overlays, func(bk bucketKey, _ map[string]*overlay) bool { return bk.bucket == bucket })
	dropped := n != len(s.overlays)
	s.mu.Unlock()
	if !dropped {
		return nil
	}
	if err := s.state.DeleteRecords(context.WithoutCancel(ctx), overlaysPrefix(bucket)); err != nil {
		return fmt.Errorf("native %q: delete the overlay records of bucket %q: %w", s.name, bucket, err)
	}
	return nil
}

// pruneOverlays drops the overlays of bucket/key stored before before
// whose version gone reports to be gone (gone runs with s.mu held and
// must not block). Unless locked (the caller holds the object's lock), it
// takes the object's lock, so it never races with gos3's writes of the
// object.
func (s *Store) pruneOverlays(ctx context.Context, bucket, key string, before time.Time, locked bool, gone func(vid string, o *overlay) bool) {
	bk := bucketKey{bucket, key}
	stale := func() []overlayID {
		var ids []overlayID
		for vid, o := range s.overlays[bk] {
			if o.at.Before(before) && gone(vid, o) {
				ids = append(ids, overlayID{bucket, key, vid})
			}
		}
		return ids
	}
	s.mu.Lock()
	ids := stale()
	s.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	if !locked {
		unlock := s.lockObject(bucket, key)
		defer unlock()
	}
	s.mu.Lock()
	ids = stale() // again, under the object's lock
	for _, id := range ids {
		s.setOverlayLocked(id, nil)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.log.Debug("native: dropping the overlay of a version removed outside gos3", "backing", s.name, "bucket", bucket, "key", key, "version", id.vid)
		s.forgetOverlay(ctx, id, &overlay{})
	}
}

// pruneVersion drops the overlay of version vid of bucket/key, which a
// lookup started at before found gone.
func (s *Store) pruneVersion(ctx context.Context, bucket, key, vid string, before time.Time, locked bool) {
	s.pruneOverlays(ctx, bucket, key, before, locked, func(v string, _ *overlay) bool { return v == vid })
}

// pruneListed drops the overlays of bucket/key whose version a complete
// version listing of the key (seen: the listed versions by gos3 version
// ID), fetched from before on, does not have (or, for the null version,
// has as another object).
func (s *Store) pruneListed(ctx context.Context, bucket, key string, before time.Time, seen map[string]s3client.ObjectAttrs) {
	s.pruneOverlays(ctx, bucket, key, before, false, func(vid string, o *overlay) bool {
		e, ok := seen[vid]
		return !ok || e.DeleteMarker || vid == storage.NullVersionID && !o.matches(vid, e.ETag, e.LastModified)
	})
}

// overlayCount returns the number of overlays of bucket's keys with the
// given prefix (tests).
func (s *Store) overlayCount(bucket, keyPrefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for bk, vs := range s.overlays {
		if bk.bucket == bucket && strings.HasPrefix(bk.key, keyPrefix) {
			n += len(vs)
		}
	}
	return n
}

// --- background sweep ---

const (
	// sweepEvery is how often the sweeper looks for buckets to sweep, and
	// sweepFirst when it looks first after Open.
	sweepEvery = time.Hour
	sweepFirst = 10 * time.Minute
	// sweepInterval is the shortest time between two sweeps of a bucket.
	sweepInterval = 24 * time.Hour
	// sweepMaxKeys bounds the keys one sweep of a bucket looks at (the
	// remote requests it sends); the next sweep continues after them.
	sweepMaxKeys = 10000
)

// sweeper runs sweepOverlays periodically until ctx is done.
func (s *Store) sweeper(ctx context.Context) {
	defer s.sweepWG.Done()
	t := time.NewTimer(sweepFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.sweepOverlays(ctx, false)
		t.Reset(sweepEvery)
	}
}

// sweepOverlays drops the overlays of versions removed outside gos3: for
// every bucket with overlays not swept for sweepInterval (all of them
// with force), it lists the versions of the keys with overlays (HEADs the
// current object of unversioned buckets) and prunes what is gone. Buckets
// the resolver does not know are skipped; a remote failure ends the
// bucket's sweep (it is tried again at the next round).
func (s *Store) sweepOverlays(ctx context.Context, force bool) {
	now := time.Now()
	s.mu.Lock()
	byBucket := map[string][]string{}
	for bk := range s.overlays {
		byBucket[bk.bucket] = append(byBucket[bk.bucket], bk.key)
	}
	for b := range byBucket {
		if !force && now.Sub(s.swept[b]) < sweepInterval {
			delete(byBucket, b)
		}
	}
	s.mu.Unlock()
	for _, bucket := range slices.Sorted(maps.Keys(byBucket)) {
		t, err := s.target(ctx, bucket)
		if err != nil {
			continue
		}
		keys := byBucket[bucket]
		slices.Sort(keys)
		s.mu.Lock()
		after := s.sweptAfter[bucket]
		s.mu.Unlock()
		i, _ := slices.BinarySearch(keys, after)
		if after != "" && i < len(keys) && keys[i] == after {
			i++
		}
		if i >= len(keys) {
			i = 0
		}
		keys = keys[i:min(len(keys), i+sweepMaxKeys)]
		done := true
		for _, key := range keys {
			if ctx.Err() != nil {
				return
			}
			if err := s.sweepKey(ctx, t, key); err != nil {
				s.log.Debug("native: overlay sweep stopped", "backing", s.name, "bucket", bucket, "err", err)
				done = false
				break
			}
		}
		s.mu.Lock()
		if done {
			s.swept[bucket] = now
			s.sweptAfter[bucket] = ""
			if len(keys) == sweepMaxKeys {
				s.sweptAfter[bucket] = keys[len(keys)-1]
			}
		}
		s.mu.Unlock()
	}
}

// sweepKey prunes the overlays of t/key whose version is gone.
func (s *Store) sweepKey(ctx context.Context, t *target, key string) error {
	before := time.Now()
	if !t.versioned() {
		var a s3client.ObjectAttrs
		err := s.call(ctx, t, func(c *s3client.Client) error {
			var err error
			a, err = c.HeadObjectWith(ctx, t.b.Remote, key, s3client.HeadOptions{})
			return err
		})
		switch {
		case err == nil:
			s.pruneOverlays(ctx, t.bucket, key, before, false, func(vid string, o *overlay) bool {
				return vid == storage.NullVersionID && !o.matches(vid, a.ETag, a.LastModified)
			})
		case missingObject(err):
			s.pruneVersion(ctx, t.bucket, key, storage.NullVersionID, before, false)
		default:
			return err
		}
		return nil
	}
	seen := map[string]s3client.ObjectAttrs{}
	err := s.rescanKey(ctx, t, key, func(e s3client.ObjectAttrs) error {
		seen[versionOf(e.VersionID)] = e
		return nil
	})
	if err != nil {
		return err
	}
	s.pruneListed(ctx, t.bucket, key, before, seen)
	return nil
}
