package copier

import (
	"context"
	"errors"
	"slices"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// SyncKey makes the target key of p hold what the source key holds, in
// identity mode: for sources whose listings report versions exactly as
// reads do (buckets gos3 stores itself), into targets that import versions
// (storage.VersionImporter; ErrNoImport otherwise, use SyncKeyFingerprint).
// It generalises the multi router's replication of a key:
//
//   - VersionsAll: the target's history of the key becomes the source's,
//     with the same version IDs (MapVersionID), modification times,
//     metadata and order. Versions are compared by identity (SameVersion;
//     SameContent unless the policy keeps encryption strictly), target
//     versions the source does not have are deleted (unless Merge, and for
//     keys the source does not have at all only when owned), the longest
//     common oldest-first prefix of both histories is kept, with its
//     metadata updated where it differs, and the rest of the target's
//     history is replaced by the rest of the source's. In the common case
//     (new versions on the source) that copies just the new versions.
//   - VersionsCurrent: the source's current version becomes the target's
//     null version, keeping its modification time and metadata; when the
//     source has no current version an owned target key loses its current
//     version (see Policy.OwnsKey).
//
// p's listing entries decide whether anything needs doing; if so the key
// is listed again on both sides and compared afresh. Deletes and metadata
// updates are conditional (they skip a version that changed since it was
// listed, and never touch a locked one, which becomes a Conflict). A
// version that changes on the source while it is copied makes the key be
// compared again, up to three more times, after which it is deferred
// (Result.Deferred).
//
// Errors are those of the backends, wrapped, and the package's sentinels
// for policies the target cannot satisfy. Changes made before an error
// stay; running the key again continues from them. A pair whose Err is
// set is not synchronised: SyncKey returns Err (and so does
// SyncKeyFingerprint).
func SyncKey(ctx context.Context, src, dst Endpoint, p KeyPair, pol Policy) (Result, error) {
	var res Result
	if err := pol.Validate(); err != nil {
		return res, err
	}
	if p.Err != nil {
		return res, p.Err // the walk could not list the key
	}
	if c, bad := keyConflict(p); bad {
		res.Conflicts = append(res.Conflicts, c)
		return res, nil
	}
	s := newSyncer(ctx, src, dst, p, pol, &res)
	if s.plan(p.Src, p.Dst).empty() {
		return res, nil
	}
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		sv, dv, err := s.list()
		if err != nil {
			return res, err
		}
		n := len(res.Conflicts)
		err = s.execute(s.plan(sv, dv))
		if !errors.Is(err, errChanged) {
			return res, err
		}
		res.Conflicts = res.Conflicts[:n] // reported again by the next attempt
		if attempt == maxAttempts {
			res.Deferred = true
			return res, nil
		}
	}
}

// plan is what identity mode does to a key, decided from both listings
// without I/O.
type plan struct {
	deletes   []delOp
	updates   []metaOp
	imports   []storage.ObjectInfo // source versions, oldest first
	conflicts []Conflict
}

// delOp deletes target version v, or hides it behind a delete marker.
type delOp struct {
	v      storage.ObjectInfo
	marker bool
}

// metaOp replaces the metadata of target version d.
type metaOp struct {
	d    storage.ObjectInfo
	want storage.ObjectMeta
}

func (pl plan) empty() bool {
	return len(pl.deletes)+len(pl.updates)+len(pl.imports)+len(pl.conflicts) == 0
}

func (s *syncer) planConflict(pl *plan, versionID, reason string) {
	pl.conflicts = append(pl.conflicts, Conflict{Key: s.p.SrcKey, Target: s.p.DstKey, VersionID: versionID, Reason: reason})
}

// dstID is the target version ID of source version v.
func (s *syncer) dstID(v storage.ObjectInfo) string {
	if !s.pol.all() {
		return storage.NullVersionID
	}
	return MapVersionID(v.VersionID)
}

// same reports whether target version d is the copy of source version v.
func (s *syncer) same(v, d storage.ObjectInfo) bool {
	v.VersionID = s.dstID(v)
	if s.pol.strictEncryption() {
		return SameVersion(v, d)
	}
	return SameContent(v, d)
}

// plan decides what to do to the key given both listings (newest first).
func (s *syncer) plan(sv, dv []storage.ObjectInfo) plan {
	if s.pol.all() {
		return s.planAll(sv, dv)
	}
	return s.planCurrent(sv, dv)
}

// planMeta adds an update of target version d's metadata to that of its
// source version v when they differ.
func (s *syncer) planMeta(pl *plan, v, d storage.ObjectInfo) {
	if v.DeleteMarker {
		return
	}
	want := s.pol.metaFor(v.ObjectMeta, d.Encryption)
	if MetaEqual(want, d.ObjectMeta) {
		return
	}
	if r := weakens(d.ObjectMeta, want, s.now); r != "" {
		s.planConflict(pl, d.VersionID, "metadata not updated: it is "+r)
		return
	}
	pl.updates = append(pl.updates, metaOp{d: d, want: want})
}

// planAll plans VersionsAll (see SyncKey). Locked target versions are
// never deleted: those the source lacks stay where they are, and a locked
// version out of the source's order is left in place with its source
// version considered copied, so the rest of the history is compared
// around it (and the next pass finds nothing more to do).
func (s *syncer) planAll(sv, dv []storage.ObjectInfo) plan {
	var pl plan
	if len(sv) == 0 && (s.pol.Merge || !s.owned) {
		return pl
	}
	src, tgt := oldestFirst(sv), oldestFirst(dv)
	byID := make(map[string]storage.ObjectInfo, len(src))
	for _, v := range src {
		byID[s.dstID(v)] = v
	}
	blocked := make(map[string]bool)
	var keep []storage.ObjectInfo
	for _, d := range tgt {
		v, ok := byID[d.VersionID]
		switch {
		case ok && s.same(v, d):
			keep = append(keep, d)
		case !ok && s.pol.Merge:
			// The source does not have it: merging keeps it.
		default:
			if r := lockReason(d, s.now); r != "" {
				blocked[d.VersionID] = true
				s.planConflict(&pl, d.VersionID, "not deleted: it is "+r)
				continue
			}
			pl.deletes = append(pl.deletes, delOp{v: d})
		}
	}
	// A blocked target version keeps its ID: its source version cannot be
	// copied.
	src = slices.DeleteFunc(src, func(v storage.ObjectInfo) bool { return blocked[s.dstID(v)] })
	if s.pol.Merge {
		have := make(map[string]storage.ObjectInfo, len(keep))
		for _, d := range keep {
			have[d.VersionID] = d
		}
		for _, v := range src {
			if d, ok := have[s.dstID(v)]; ok {
				s.planMeta(&pl, v, d)
				continue
			}
			pl.imports = append(pl.imports, v)
		}
		return pl
	}
	for {
		common := 0
		for common < len(keep) && common < len(src) && keep[common].VersionID == s.dstID(src[common]) {
			common++
		}
		i := slices.IndexFunc(keep[common:], func(d storage.ObjectInfo) bool { return lockReason(d, s.now) != "" })
		if i < 0 {
			for _, d := range keep[common:] {
				pl.deletes = append(pl.deletes, delOp{v: d})
			}
			for j := range common {
				s.planMeta(&pl, src[j], keep[j])
			}
			pl.imports = append(pl.imports, src[common:]...)
			return pl
		}
		d := keep[common+i]
		s.planConflict(&pl, d.VersionID, "out of the source's order, and not moved: it is "+lockReason(d, s.now))
		keep = slices.Delete(keep, common+i, common+i+1)
		src = slices.DeleteFunc(src, func(v storage.ObjectInfo) bool { return s.dstID(v) == d.VersionID })
	}
}

// planCurrent plans VersionsCurrent (see SyncKey).
func (s *syncer) planCurrent(sv, dv []storage.ObjectInfo) plan {
	var pl plan
	cur, dcur := current(sv), current(dv)
	switch {
	case cur == nil:
		if dcur == nil || s.pol.Merge || !s.owned {
			return pl
		}
		if dcur.VersionID != storage.NullVersionID {
			// A version the copy did not write (as the target's null
			// version): hide it, keeping the target's history.
			pl.deletes = append(pl.deletes, delOp{v: *dcur, marker: true})
			return pl
		}
		if r := lockReason(*dcur, s.now); r != "" {
			s.planConflict(&pl, dcur.VersionID, "not deleted: it is "+r)
			return pl
		}
		pl.deletes = append(pl.deletes, delOp{v: *dcur})
	case dcur != nil && dcur.VersionID == storage.NullVersionID && s.same(*cur, *dcur):
		s.planMeta(&pl, *cur, *dcur)
	default:
		pl.imports = append(pl.imports, *cur)
	}
	return pl
}

// execute carries out a plan: deletes first, then metadata updates, then
// copies, oldest first. It returns errChanged when the key must be
// compared again.
func (s *syncer) execute(pl plan) error {
	s.res.Conflicts = append(s.res.Conflicts, pl.conflicts...)
	blocked := make(map[string]bool)
	for _, op := range pl.deletes {
		locked, err := s.deleteVersion(op.v, op.marker)
		if err != nil {
			return err
		}
		if locked {
			blocked[op.v.VersionID] = true
		}
	}
	changed := false
	for _, op := range pl.updates {
		if err := s.updateMeta(op.d, op.want); err != nil {
			if !errors.Is(err, errChanged) {
				return err
			}
			changed = true
		}
	}
	for _, v := range pl.imports {
		id := s.dstID(v)
		if blocked[id] {
			continue
		}
		if !s.pol.all() {
			// The import replaces the target's null version wherever it is.
			r, err := s.versionLocked(storage.NullVersionID)
			if err != nil {
				return err
			}
			if r != "" {
				s.conflict(storage.NullVersionID, "not replaced: it is "+r)
				continue
			}
		}
		if _, err := s.copyVersion(v, id, WriteImport); err != nil {
			if !errors.Is(err, errChanged) {
				return err
			}
			changed = true
		}
	}
	if changed {
		return errChanged
	}
	return nil
}
