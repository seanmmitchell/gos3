package copier

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Record is what fingerprint mode remembers about one key after copying
// it (see SyncKeyFingerprint).
type Record struct {
	// SrcFP and DstFP fingerprint the source's and the target's listings
	// of the key (Fingerprint) as they were when the key was last copied.
	SrcFP string `json:"src,omitempty"`
	DstFP string `json:"dst,omitempty"`
	// Owned: the copy wrote the key, so it may delete it once the source
	// has none of it (see Policy.OwnsKey).
	Owned bool `json:"owned,omitempty"`
	// VIDMap maps each copied source version (its VersionToken: the
	// source version ID, "@" and a short fingerprint of its listing entry,
	// since a null version can be overwritten in place) to the ID of its
	// copy on the target: the same ID or MapVersionID's remapping of it
	// when the target imports, the target's own ID for a plain write. Only
	// kept with VersionsAll.
	VIDMap map[string]string `json:"vids,omitempty"`
}

// FingerprintStore keeps the Records of one copy (a job's database), by
// source key. Implementations must be safe for concurrent use on different
// keys.
type FingerprintStore interface {
	// Get returns the record of key, and false when there is none.
	Get(key string) (Record, bool, error)
	Put(key string, r Record) error
	// Delete removes the record of key; removing a missing one is not an
	// error.
	Delete(key string) error
}

// MemStore is a FingerprintStore in memory, for copies without a
// database.
type MemStore struct {
	mu sync.Mutex
	m  map[string]Record
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore { return &MemStore{m: make(map[string]Record)} }

// Get implements FingerprintStore.
func (s *MemStore) Get(key string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[key]
	r.VIDMap = maps.Clone(r.VIDMap)
	return r, ok, nil
}

// Put implements FingerprintStore.
func (s *MemStore) Put(key string, r Record) error {
	r.VIDMap = maps.Clone(r.VIDMap)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = r
	return nil
}

// Delete implements FingerprintStore.
func (s *MemStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

// Len returns the number of records.
func (s *MemStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// Fingerprint returns a fingerprint of a listing of one key: of each
// entry's ETag, size and modification time, plus its version ID and
// delete-marker flag when versions is set (a walk over versions; without
// it vs is a walk over objects, at most the latest version). It is "" for
// an empty listing. Fingerprints are only compared with fingerprints of
// the same side's listings, so a listing that reports other data than
// reads (a multipart ETag) does not matter.
func Fingerprint(vs []storage.ObjectInfo, versions bool) string {
	if len(vs) == 0 {
		return ""
	}
	h := sha256.New()
	for _, v := range vs {
		if versions {
			fmt.Fprintf(h, "%d:%s|%t|", len(v.VersionID), v.VersionID, v.DeleteMarker)
		}
		fmt.Fprintf(h, "%d:%s|%d|%d.%09d\n", len(v.ETag), v.ETag, v.Size, v.LastModified.Unix(), v.LastModified.Nanosecond())
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// VersionToken identifies a source version in Record.VIDMap: its version
// ID, "@" and twelve hex digits of a hash of its listing entry (ETag,
// size, modification time, delete-marker flag).
func VersionToken(v storage.ObjectInfo) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d:%s|%d|%d.%09d|%t", len(v.ETag), v.ETag, v.Size, v.LastModified.Unix(), v.LastModified.Nanosecond(), v.DeleteMarker)
	return v.VersionID + "@" + hex.EncodeToString(h.Sum(nil)[:6])
}

// dirtyFP is the SrcFP of a record whose copy did not complete: it matches
// no listing, so the key is copied again.
const dirtyFP = "-"

// listingFP fingerprints a listing of either kind for the policy.
func listingFP(vs []storage.ObjectInfo, all bool) string {
	if all {
		return Fingerprint(vs, true)
	}
	if c := current(vs); c != nil {
		return Fingerprint([]storage.ObjectInfo{*c}, false)
	}
	return ""
}

// SyncKeyFingerprint makes the target key of p hold what the source key
// holds, in fingerprint mode: for native sources, whose listings may
// report other data than reads, and for targets that may not import
// versions (native buckets). It never compares one side with the other.
// The key is copied when the store has no record of it, when the source's
// listing no longer has the fingerprint recorded (it changed), or when the
// target's listing does not (the copy was changed or removed behind the
// copy's back); otherwise nothing is read.
//
// Copies are imported where the target can (keeping version IDs, mapped
// by MapVersionID, and modification times), else written plainly
// (WriteVersion's WriteImportOrPlain):
//
//   - VersionsAll: the source's versions are written oldest first and the
//     store maps each one to its copy (Record.VIDMap), so later passes copy
//     only new versions, delete the copies of versions the source lost
//     (unless Merge) and rewrite the copies that are out of order. Plain
//     writes need a target with versioning enabled (ErrNotVersioned).
//     Target versions that are not copies are deleted unless Merge.
//   - VersionsCurrent: the source's current version is written (imported
//     as the null version, or written plainly).
//
// When the source has none of the key (no version with VersionsAll, no
// current version with VersionsCurrent), an owned target key (one the
// record says the copy wrote, or Policy.OwnsKey) is deleted as SyncKey
// deletes it, unless Merge; then the record is removed.
//
// The source fingerprint recorded is that of the listing the copy started
// from, so a change during the copy is copied by the next pass. Without a
// record, a target version that matches (by ID, size and modification
// time) a source version on a target that imports is adopted instead of
// copied again. Locked target versions are never deleted or replaced
// (Conflicts); a key that keeps changing is deferred, as with SyncKey.
func SyncKeyFingerprint(ctx context.Context, src, dst Endpoint, p KeyPair, st FingerprintStore, pol Policy) (Result, error) {
	var res Result
	if err := pol.Validate(); err != nil {
		return res, err
	}
	if st == nil {
		return res, errors.New("copier: nil fingerprint store")
	}
	if p.Err != nil {
		return res, p.Err // the walk could not list the key
	}
	if c, bad := keyConflict(p); bad {
		res.Conflicts = append(res.Conflicts, c)
		return res, nil
	}
	all := pol.all()
	rec, found, err := st.Get(p.SrcKey)
	if err != nil {
		return res, fmt.Errorf("reading the record of %s: %w", p.SrcKey, err)
	}
	s := newSyncer(ctx, src, dst, p, pol, &res)
	s.fingerprint = true
	s.owned = s.owned || found && rec.Owned
	srcFP, dstFP := listingFP(p.Src, all), listingFP(p.Dst, all)
	switch {
	case found && rec.SrcFP == srcFP && rec.DstFP == dstFP:
		return res, nil
	case srcFP == "" && dstFP == "":
		if found {
			return res, st.Delete(p.SrcKey)
		}
		return res, nil
	case srcFP == "" && (pol.Merge || !s.owned):
		return res, nil
	}
	if !found {
		rec = Record{}
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
		remove, err := s.syncFingerprint(sv, dv, &rec, !found)
		switch {
		case err == nil:
			if remove {
				return res, st.Delete(p.SrcKey)
			}
			return res, st.Put(p.SrcKey, rec)
		case errors.Is(err, errChanged) && attempt < maxAttempts:
			res.Conflicts = res.Conflicts[:n]
			continue
		case errors.Is(err, errChanged):
			res.Deferred = true
			err = nil
		}
		// Keep what was copied (the version map), but copy the key again.
		if rec.Owned || len(rec.VIDMap) > 0 {
			rec.SrcFP = dirtyFP
			if perr := st.Put(p.SrcKey, rec); perr != nil && err == nil {
				err = perr
			}
		}
		return res, err
	}
}

// syncFingerprint brings the target in line with fresh listings of the
// key, updating rec. remove: the record is to be deleted (the source and,
// as far as the copy may delete, the target have none of the key).
func (s *syncer) syncFingerprint(sv, dv []storage.ObjectInfo, rec *Record, first bool) (remove bool, err error) {
	all := s.pol.all()
	srcFP := listingFP(sv, all)
	if srcFP == "" {
		if s.pol.Merge || !s.owned {
			// Not ours to delete; the record (if any) is kept.
			rec.SrcFP, rec.DstFP = "", listingFP(dv, all)
			return false, nil
		}
		n := len(s.res.Conflicts)
		if err := s.execute(s.plan(nil, dv)); err != nil {
			return false, err
		}
		if len(s.res.Conflicts) == n {
			return true, nil
		}
		// Locked versions stay: remember them, so they are not tried
		// again until something changes.
		rec.VIDMap = nil
		return false, s.recordTarget(rec, "")
	}
	wrote := s.res.VersionsCopied + s.res.VersionsDeleted
	if all {
		err = s.fpAll(sv, dv, rec, first)
	} else {
		err = s.fpCurrent(sv, dv, first)
	}
	// The key is the copy's once it holds (or held) the copy.
	if err == nil || s.res.VersionsCopied+s.res.VersionsDeleted > wrote {
		rec.Owned = true
	}
	if err != nil {
		return false, err
	}
	return false, s.recordTarget(rec, srcFP)
}

// recordTarget sets rec's fingerprints: srcFP and that of a fresh listing
// of the target.
func (s *syncer) recordTarget(rec *Record, srcFP string) error {
	dv, err := listKey(s.ctx, s.dst.Backend, s.dst.Bucket, s.p.DstKey, s.pol.all())
	if err != nil {
		return fmt.Errorf("listing %s on the target %s: %w", s.p.DstKey, s.dst, err)
	}
	rec.SrcFP, rec.DstFP = srcFP, listingFP(dv, s.pol.all())
	return nil
}

// fpCurrent copies the source's current version (VersionsCurrent).
func (s *syncer) fpCurrent(sv, dv []storage.ObjectInfo, first bool) error {
	cur, dcur := *current(sv), current(dv)
	imports := s.canImport()
	if first && imports && dcur != nil {
		as := cur
		as.VersionID = storage.NullVersionID
		if sameListing(as, *dcur) {
			return nil // a copy is there already
		}
	}
	if s.importer() {
		// An import replaces the target's null version wherever it is.
		r, err := s.versionLocked(storage.NullVersionID)
		if err != nil {
			return err
		}
		if r != "" {
			s.conflict(storage.NullVersionID, "not replaced: it is "+r)
			return nil
		}
	}
	_, err := s.copyVersion(cur, storage.NullVersionID, WriteImportOrPlain)
	return err
}

// fpAll copies the source's history (VersionsAll), keeping rec.VIDMap.
func (s *syncer) fpAll(sv, dv []storage.ObjectInfo, rec *Record, first bool) error {
	src, tgt := oldestFirst(sv), oldestFirst(dv)
	if rec.VIDMap == nil {
		rec.VIDMap = make(map[string]string)
	}
	m := rec.VIDMap
	toks := make([]string, len(src))
	live := make(map[string]bool, len(src))
	for i, v := range src {
		toks[i] = VersionToken(v)
		live[toks[i]] = true
	}
	for tok := range m {
		if !live[tok] {
			delete(m, tok) // the source lost that version
		}
	}
	tByID := make(map[string]storage.ObjectInfo, len(tgt))
	for _, t := range tgt {
		tByID[t.VersionID] = t
	}
	imports := s.canImport()
	adopt := first && imports
	// img[i] is the ID of the target version holding the copy of src[i]
	// ("" = none); owner maps it back.
	img := make([]string, len(src))
	owner := make(map[string]int, len(src))
	for i, v := range src {
		id, ok := m[toks[i]]
		if ok {
			if t, found := tByID[id]; !found || t.DeleteMarker != v.DeleteMarker {
				delete(m, toks[i]) // the copy is gone
				ok = false
			}
		} else if adopt {
			id = MapVersionID(v.VersionID)
			as := v
			as.VersionID = id
			if t, found := tByID[id]; found && sameListing(as, t) {
				m[toks[i]], ok = id, true
			}
		}
		if !ok {
			continue
		}
		if j, dup := owner[id]; dup {
			img[j] = "" // two claims on one copy: the newer wins
			delete(m, toks[j])
		}
		img[i], owner[id] = id, i
	}

	// The IDs imports of the source's versions would replace.
	wanted := make(map[string]bool, len(src))
	for _, v := range src {
		wanted[MapVersionID(v.VersionID)] = true
	}
	importer := s.importer()
	blocked := make(map[string]bool)
	var (
		deletes []delOp
		keep    []storage.ObjectInfo // target versions holding copies, oldest first
	)
	for _, t := range tgt {
		if i, ok := owner[t.VersionID]; ok && img[i] == t.VersionID {
			keep = append(keep, t)
			continue
		}
		if s.pol.Merge {
			// Kept, unless an import would replace it: never a locked one.
			if importer && wanted[t.VersionID] {
				if r := lockReason(t, s.now); r != "" {
					blocked[t.VersionID] = true
					s.conflict(t.VersionID, "not replaced: it is "+r)
				}
			}
			continue
		}
		if r := lockReason(t, s.now); r != "" {
			blocked[t.VersionID] = true
			s.conflict(t.VersionID, "not deleted: it is "+r)
			continue
		}
		deletes = append(deletes, delOp{v: t})
	}
	order := make([]int, len(src)) // source versions still to place, oldest first
	for i := range order {
		order[i] = i
	}
	var writes []int
	if s.pol.Merge {
		for _, i := range order {
			if img[i] == "" {
				writes = append(writes, i)
			}
		}
	} else {
		for {
			common := 0
			for common < len(keep) && common < len(order) && keep[common].VersionID == img[order[common]] {
				common++
			}
			k := slices.IndexFunc(keep[common:], func(t storage.ObjectInfo) bool { return lockReason(t, s.now) != "" })
			if k < 0 {
				for _, t := range keep[common:] {
					deletes = append(deletes, delOp{v: t})
				}
				writes = order[common:]
				break
			}
			t := keep[common+k]
			s.conflict(t.VersionID, "out of the source's order, and not moved: it is "+lockReason(t, s.now))
			keep = slices.Delete(keep, common+k, common+k+1)
			order = slices.DeleteFunc(order, func(i int) bool { return i == owner[t.VersionID] })
		}
	}

	for _, op := range deletes {
		locked, err := s.deleteVersion(op.v, false)
		if err != nil {
			return err
		}
		if locked {
			blocked[op.v.VersionID] = true
			continue
		}
		delete(tByID, op.v.VersionID) // gone: an import of its ID replaces nothing
		if i, ok := owner[op.v.VersionID]; ok {
			delete(m, toks[i])
		}
	}
	if len(writes) == 0 {
		return nil
	}
	info, err := s.w.bucketInfo(s.ctx)
	if err != nil {
		return fmt.Errorf("reading the target bucket %q: %w", s.dst.Bucket, err)
	}
	mode := WriteImport
	if info.Versioning == storage.VersioningEnabled {
		mode = WriteImportOrPlain
	}
	rev := make(map[string]string, len(m)) // target version ID -> token
	for tok, id := range m {
		rev[id] = tok
	}
	changed := false
	for _, i := range writes {
		v := src[i]
		id := MapVersionID(v.VersionID)
		if importer {
			// An import replaces a version with its ID: never a locked one
			// (checked afresh, as the listing may be old or lack locks).
			if blocked[id] {
				continue
			}
			if _, ok := tByID[id]; ok {
				r, err := s.versionLocked(id)
				if err != nil {
					return err
				}
				if r != "" {
					blocked[id] = true
					s.conflict(id, "not replaced: it is "+r)
					continue
				}
			}
		}
		wr, err := s.copyVersion(v, id, mode)
		if errors.Is(err, errChanged) {
			changed = true
			continue
		}
		if err != nil {
			if mode == WriteImport && errors.Is(err, ErrNoImport) {
				return fmt.Errorf("%w (bucket %q): %w", ErrNotVersioned, s.dst.Bucket, err)
			}
			return err
		}
		got := wr.Info.VersionID
		if old, ok := rev[got]; ok && old != toks[i] {
			delete(m, old) // that copy was overwritten
		}
		m[toks[i]], rev[got] = got, toks[i]
	}
	if changed {
		return errChanged
	}
	return nil
}
