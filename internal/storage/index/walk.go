package index

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// The walk helpers read a bucket in batches. For each batch they call
// acquire, which returns the bucket locked for reading (looked up afresh,
// so a bucket deleted meanwhile ends the walk with acquire's error) and the
// function that unlocks it; they collect the batch, unlock, and only then
// call the walk function, which may therefore call back into the backend,
// including mutating it. fn must not be nil.

const (
	walkMinBatch = 16
	walkMaxBatch = 1024
)

// nextBatch grows walk batches geometrically so that walks stopped early
// (the list helpers stop after one item on every common prefix) copy little,
// while long walks amortise locking.
func nextBatch(n int) int {
	if n*2 > walkMaxBatch {
		return walkMaxBatch
	}
	return n * 2
}

// callWalk invokes fn for each item; it reports stop=true when fn asked to
// stop (err nil) or failed (err non-nil).
func callWalk[T any](items []T, fn func(T) error) (bool, error) {
	for _, it := range items {
		if err := fn(it); err != nil {
			if errors.Is(err, storage.ErrStopWalk) {
				return true, nil
			}
			return true, err
		}
	}
	return false, nil
}

// WalkObjects implements storage.Backend.WalkObjects: it calls fn with the
// latest version of each key (skipping keys whose latest version is a
// delete marker) in ascending key order, for keys with the given prefix
// that are strictly greater than after.
func WalkObjects[P any](ctx context.Context, acquire func() (*Bucket[P], func(), error), prefix, after string, fn func(storage.ObjectInfo) error) error {
	cursor := after
	inclusive := false // seek > cursor, or >= prefix when cursor < prefix
	if after < prefix {
		cursor, inclusive = prefix, true
	}
	batch := walkMinBatch
	for {
		bk, unlock, err := acquire()
		if err != nil {
			return err
		}
		var n *slNode[*Object[P]]
		if inclusive {
			n = bk.objects.SeekGE(cursor)
		} else {
			n = bk.objects.SeekGT(cursor)
		}
		items := make([]storage.ObjectInfo, 0, batch)
		scanned, done := 0, true
		for ; n != nil; n = n.Next() {
			if !strings.HasPrefix(n.key, prefix) {
				break
			}
			if len(items) == batch || scanned == 4*batch {
				done = false
				break
			}
			scanned++
			cursor, inclusive = n.key, false
			if l := n.val.Latest(); !l.DeleteMarker {
				items = append(items, l.Info(bk.Info.Name, n.key, true))
			}
		}
		unlock()

		if stop, err := callWalk(items, fn); stop || err != nil {
			return err
		}
		if done {
			return nil
		}
		if err := checkCtx(ctx); err != nil {
			return err
		}
		batch = nextBatch(batch)
	}
}

// WalkVersions implements storage.Backend.WalkVersions: it calls fn for
// every version and delete marker of keys with the given prefix, keys
// ascending and newest version first within a key, starting after
// (keyMarker, versionIDMarker) per S3 ListObjectVersions semantics. If
// versionIDMarker is not a version of keyMarker, the walk resumes at the
// position the ID would occupy when it is a generated ID (e.g. a version
// deleted between pages), with every version of keyMarker when it is
// "null" (a deleted null version has no position), and otherwise after all
// versions of keyMarker. IsLatest marks at most the first entry delivered
// for a key.
func WalkVersions[P any](ctx context.Context, acquire func() (*Bucket[P], func(), error), prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	// Position: either strictly after all versions of key (below == 0) or
	// at the versions of key whose stamp is < below. inclusive means "seek
	// to the first key >= key" (initial position at the prefix).
	//
	// midKey is set when a batch ended inside the versions of key: the
	// newest of them was already delivered, so none of the remaining ones
	// is reported as latest even if newer versions were deleted meanwhile
	// (IsLatest marks at most the first entry delivered for a key).
	var (
		key       string
		below     uint64
		inclusive bool
		midKey    bool
	)
	switch {
	case keyMarker == "" || keyMarker < prefix:
		key, inclusive = prefix, true
	case versionIDMarker == "":
		key = keyMarker
	default:
		key = keyMarker
		bk, unlock, err := acquire()
		if err != nil {
			return err
		}
		if obj, ok := bk.objects.Get(keyMarker); ok {
			if i := obj.indexByID(versionIDMarker); i >= 0 {
				below = obj.versions[i].Stamp
			}
		}
		unlock()
		if below == 0 {
			if s, ok := StampFromVersionID(versionIDMarker); ok {
				below = s
			} else if versionIDMarker == storage.NullVersionID {
				// The null version was deleted (e.g. between pages) and
				// "null" carries no position: resume with every remaining
				// version of keyMarker rather than lose the older ones.
				inclusive = true
			}
		}
	}

	batch := walkMinBatch
	for {
		bk, unlock, err := acquire()
		if err != nil {
			return err
		}
		items := make([]storage.ObjectInfo, 0, batch)
		done := true
		var n *slNode[*Object[P]]
		switch {
		case inclusive:
			n = bk.objects.SeekGE(key)
		case below != 0:
			n = bk.objects.SeekGE(key)
			if n != nil && n.key != key {
				// key vanished; continue with the following keys.
				below = 0
			}
		default:
			n = bk.objects.SeekGT(key)
		}
	scan:
		for ; n != nil; n = n.Next() {
			if !strings.HasPrefix(n.key, prefix) {
				break
			}
			if len(items) == batch {
				// Resume at n: the current position (after the previous key,
				// or within the marker key) is still accurate.
				done = false
				break
			}
			vs := n.val.versions
			top := len(vs) - 1
			continued := false
			if below != 0 && n.key == key {
				// Only versions strictly older than the marker.
				top = sort.Search(len(vs), func(i int) bool { return vs[i].Stamp >= below }) - 1
				continued = midKey
			}
			for j := top; j >= 0; j-- {
				items = append(items, vs[j].Info(bk.Info.Name, n.key, !continued && j == len(vs)-1))
				if len(items) == batch && j > 0 {
					key, below, inclusive, midKey = n.key, vs[j].Stamp, false, true
					done = false
					break scan
				}
			}
			key, below, inclusive, midKey = n.key, 0, false, false
		}
		unlock()

		if stop, err := callWalk(items, fn); stop || err != nil {
			return err
		}
		if done {
			return nil
		}
		if err := checkCtx(ctx); err != nil {
			return err
		}
		batch = nextBatch(batch)
	}
}
