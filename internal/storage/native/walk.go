package native

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Walks
//
// Unversioned buckets are walked with ListObjectsV2: every object is its
// null version. Versioned (and versioning-suspended) buckets are walked
// with ListObjectVersions, whose entries keep the remote's order (keys
// ascending, newest first per key) and version IDs; WalkObjects keeps the
// first entry of each key unless it is a delete marker.
//
// IsLatest is the remote's flag on the first entry delivered for a key and
// false on every later one, also when a key's versions span two pages and
// the key changed in between.
//
// Resuming within a key (WalkVersions with a version-ID marker) relies on
// the remote, and services differ when the marker version was deleted in
// the meantime: gos3 resumes at the deleted version's position, AWS skips
// to the next key or answers 400. When the remote answers 400, or the
// first entry it returns is not for the key marker and a HEAD of the
// marker version finds it gone, the walk lists the key marker's remaining
// versions again from its newest: versions may be delivered twice, none
// is skipped.
//
// Markers that are not valid UTF-8 (storage.ListObjects restarts after
// prefix+"\xff" to skip a common prefix) are replaced by a valid lower
// bound (startAfter) and the keys in between filtered out locally.

// WalkObjects calls fn with the latest version of each key whose latest
// version is not a delete marker (see storage.Backend). Entries carry the
// listing's metadata only (see listedInfo).
func (s *Store) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("native: nil walk function")
	}
	t, err := s.target(ctx, bucket)
	if err != nil {
		return err
	}
	if !t.versioned() {
		return s.walkCurrent(ctx, t, prefix, after, fn)
	}
	return s.walkVersions(ctx, t, prefix, after, "", "", func(e s3client.ObjectAttrs, first bool) error {
		if !first || e.DeleteMarker {
			return nil
		}
		info := s.listedInfo(t, e.Key, e)
		info.IsLatest = true
		return fn(info)
	})
}

// WalkVersions calls fn for every version and delete marker (see
// storage.Backend and "Walks" above).
func (s *Store) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("native: nil walk function")
	}
	t, err := s.target(ctx, bucket)
	if err != nil {
		return err
	}
	return s.walkVersionInfos(ctx, t, prefix, keyMarker, versionIDMarker, "", fn)
}

// walkVersionInfos implements WalkVersions on target t. held is a key
// whose object lock the caller holds ("" for none): pruning its overlays
// must not take the lock again (see walkVersions).
func (s *Store) walkVersionInfos(ctx context.Context, t *target, prefix, keyMarker, versionIDMarker, held string, fn func(storage.ObjectInfo) error) error {
	if !t.versioned() {
		// Only null versions: every version-ID marker positions after
		// keyMarker's null version, i.e. after keyMarker.
		return s.walkCurrent(ctx, t, prefix, keyMarker, fn)
	}
	if keyMarker == "" {
		versionIDMarker = ""
	}
	return s.walkVersions(ctx, t, prefix, keyMarker, versionIDMarker, held, func(e s3client.ObjectAttrs, first bool) error {
		info := s.listedInfo(t, e.Key, e)
		info.IsLatest = e.IsLatest && first
		return fn(info)
	})
}

// walkCurrent walks an unversioned bucket's objects (ListObjectsV2) whose
// key has the prefix and sorts after after, as null versions.
func (s *Store) walkCurrent(ctx context.Context, t *target, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if impossiblePrefix(prefix) {
		return nil
	}
	in := s3client.ListInput{Prefix: prefix[:validPrefixLen(prefix)], StartAfter: startAfter(after), MaxKeys: s.pageSize()}
	for {
		var page s3client.ListOutput
		fetched := time.Now()
		err := s.call(ctx, t, func(c *s3client.Client) error {
			var err error
			page, err = c.ListObjectsV2(ctx, t.b.Remote, in)
			return err
		})
		if err != nil {
			return s.remoteErr("ListObjectsV2", t, "", bucketOp, err)
		}
		for _, o := range page.Objects {
			if prefix != "" && o.Key > prefix && !strings.HasPrefix(o.Key, prefix) {
				return nil
			}
			if !utf8.ValidString(o.Key) || !strings.HasPrefix(o.Key, prefix) || o.Key <= after {
				continue
			}
			o.VersionID, o.IsLatest = "", true
			if s.hasOverlays(t.bucket, o.Key) {
				// An overlay of the null version made for another object
				// never applies again.
				s.pruneOverlays(ctx, t.bucket, o.Key, fetched, false, func(vid string, ov *overlay) bool {
					return vid == storage.NullVersionID && !ov.matches(vid, o.ETag, o.LastModified)
				})
			}
			if err := fn(s.listedInfo(t, o.Key, o)); err != nil {
				if errors.Is(err, storage.ErrStopWalk) {
					return nil
				}
				return err
			}
		}
		if !page.IsTruncated {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		in.ContinuationToken = page.NextContinuationToken
	}
}

// walkVersions calls fn with every ListObjectVersions entry of the keys
// with the prefix after the position (keyMarker, vidMarker) as defined by
// storage.Backend.WalkVersions; first is set on the first entry delivered
// for each key. fn returning storage.ErrStopWalk ends the walk with nil.
//
// A key whose versions were all listed proves the versions it does not
// have gone: their overlays are pruned (pruneListed) once the walk moved
// past the key. Pruning takes the key's object lock, except for held, a
// key whose lock the caller holds already (the lock is not reentrant).
func (s *Store) walkVersions(ctx context.Context, t *target, prefix, keyMarker, vidMarker, held string,
	fn func(e s3client.ObjectAttrs, first bool) error) error {
	if impossiblePrefix(prefix) {
		return nil
	}
	var (
		lastKey  string
		haveLast bool
		fetched  time.Time // when the request that returned the entries was sent
		resuming = vidMarker != ""
		track    *keyTrack // the current key's versions, when it has overlays
	)
	finish := func() {
		if track != nil && track.complete {
			s.pruneListed(ctx, t.bucket, track.key, track.since, track.seen, track.key == held)
		}
		track = nil
	}
	deliver := func(e s3client.ObjectAttrs) error {
		first := !haveLast || e.Key != lastKey
		if first {
			finish()
			if s.hasOverlays(t.bucket, e.Key) {
				track = &keyTrack{key: e.Key, since: fetched, complete: !resuming || e.Key != keyMarker, seen: map[string]s3client.ObjectAttrs{}}
			}
		}
		if track != nil {
			track.seen[versionOf(e.VersionID)] = e
		}
		lastKey, haveLast = e.Key, true
		return fn(e, first)
	}
	rescan := func() error {
		// keyMarker's versions are listed again from its newest.
		resuming, fetched = false, time.Now()
		return s.rescanKey(ctx, t, keyMarker, deliver)
	}
	in := s3client.VersionListInput{Prefix: prefix[:validPrefixLen(prefix)], MaxKeys: s.pageSize()}
	exact := keyMarker != "" && validPrefixLen(keyMarker) == len(keyMarker) && xmlSafe(keyMarker)
	switch {
	case exact:
		in.KeyMarker, in.VersionIDMarker = keyMarker, vidMarker
	case keyMarker != "":
		in.KeyMarker, vidMarker = startAfter(keyMarker), ""
	}
	// Entries of keyMarker come after a version-ID marker only.
	keep := func(key string) bool {
		if !utf8.ValidString(key) || !strings.HasPrefix(key, prefix) {
			return false
		}
		return key > keyMarker || key == keyMarker && vidMarker != ""
	}
	checkMarker := vidMarker != "" && strings.HasPrefix(keyMarker, prefix)
	err := func() error {
		for {
			var page s3client.VersionListOutput
			fetched = time.Now()
			err := s.call(ctx, t, func(c *s3client.Client) error {
				var err error
				page, err = c.ListObjectVersions(ctx, t.b.Remote, in)
				return err
			})
			if err != nil && in.VersionIDMarker != "" && checkMarker && badRequest(err) {
				// The remote refuses the marker (a deleted version).
				checkMarker = false
				if err := rescan(); err != nil {
					return err
				}
				in.VersionIDMarker = ""
				continue
			}
			if err != nil {
				return s.remoteErr("ListObjectVersions", t, "", bucketOp, err)
			}
			if checkMarker {
				checkMarker = false
				if len(page.Entries) == 0 || page.Entries[0].Key != keyMarker {
					gone, err := s.versionGone(ctx, t, keyMarker, vidMarker)
					if err != nil {
						return err
					}
					if gone {
						// The remote may have skipped the rest of the
						// key: list it again.
						if err := rescan(); err != nil {
							return err
						}
					}
				}
			}
			for _, e := range page.Entries {
				if prefix != "" && e.Key > prefix && !strings.HasPrefix(e.Key, prefix) {
					finish()
					return nil
				}
				if !keep(e.Key) {
					continue
				}
				if err := deliver(e); err != nil {
					return err
				}
			}
			if !page.IsTruncated || page.NextKeyMarker == "" {
				finish()
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			in.KeyMarker, in.VersionIDMarker = page.NextKeyMarker, page.NextVersionIDMarker
		}
	}()
	if errors.Is(err, storage.ErrStopWalk) {
		return nil
	}
	return err
}

// keyTrack collects the versions a walk listed of one key (see
// walkVersions).
type keyTrack struct {
	key      string
	since    time.Time // when the first request listing the key was sent
	complete bool      // listed from its newest version
	seen     map[string]s3client.ObjectAttrs
}

// rescanKey delivers every version of key, newest first.
func (s *Store) rescanKey(ctx context.Context, t *target, key string, deliver func(s3client.ObjectAttrs) error) error {
	in := s3client.VersionListInput{Prefix: key, MaxKeys: s.pageSize()}
	for {
		var page s3client.VersionListOutput
		err := s.call(ctx, t, func(c *s3client.Client) error {
			var err error
			page, err = c.ListObjectVersions(ctx, t.b.Remote, in)
			return err
		})
		if err != nil {
			return s.remoteErr("ListObjectVersions", t, key, bucketOp, err)
		}
		for _, e := range page.Entries {
			if e.Key != key {
				return nil
			}
			if err := deliver(e); err != nil {
				return err
			}
		}
		if !page.IsTruncated || page.NextKeyMarker == "" {
			return nil
		}
		in.KeyMarker, in.VersionIDMarker = page.NextKeyMarker, page.NextVersionIDMarker
	}
}

// versionGone reports whether version vid of key no longer exists.
func (s *Store) versionGone(ctx context.Context, t *target, key, vid string) (bool, error) {
	err := s.call(ctx, t, func(c *s3client.Client) error {
		_, err := c.HeadObjectWith(ctx, t.b.Remote, key, s3client.HeadOptions{VersionID: vid})
		return err
	})
	st, _ := statusOf(err)
	_, marker := s3client.IsDeleteMarker(err)
	switch {
	case err == nil, marker, st == http.StatusMethodNotAllowed:
		return false, nil
	case missingVersion(err):
		return true, nil
	}
	return false, s.remoteErr("HeadObject", t, key, objectOp, err)
}

// badRequest reports a 400 answer that is not about the region.
func badRequest(err error) bool {
	if _, wrong := s3client.IsWrongRegion(err); wrong {
		return false
	}
	st, _ := statusOf(err)
	return st == http.StatusBadRequest
}

// pageSize is the page size of listings (0 = the service's default).
func (s *Store) pageSize() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listPage
}

// maxCodePoint is the largest Unicode code point; no valid UTF-8 string
// that starts with base+maxCodePoint+... sorts below base+maxCodePoint
// except base+maxCodePoint itself.
const maxCodePoint = "\U0010FFFF"

// impossiblePrefix rejects malformed bytes, while retaining byte-prefix
// semantics for a prefix ending part-way through a valid UTF-8 rune.
func impossiblePrefix(prefix string) bool {
	i := validPrefixLen(prefix)
	return i < len(prefix) && utf8.FullRuneInString(prefix[i:])
}

// validPrefixLen returns the length of the longest valid UTF-8 prefix of s.
func validPrefixLen(s string) int {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n <= 1 {
			return i
		}
		i += n
	}
	return len(s)
}

// startAfter returns a valid UTF-8 lower bound for keys > after: every
// key > after is > the bound. Remotes reject invalid UTF-8 in start-after
// and key-marker, but internal/storage/list.go restarts walks after
// p+"\xff" to skip the keys under a common prefix p. When after has an
// invalid byte at position i that can never start a UTF-8 sequence
// (0xF5..0xFF), every valid key with prefix after[:i] that sorts before
// after has a code point <= U+10FFFF at i, so after[:i]+U+10FFFF is a
// tight bound; for other invalid bytes the bound is after[:i]. Keys
// between the bound and after are skipped by the caller.
func startAfter(after string) string {
	i := validPrefixLen(after)
	if i == len(after) {
		return after
	}
	if after[i] >= 0xF5 {
		return after[:i] + maxCodePoint
	}
	return after[:i]
}
