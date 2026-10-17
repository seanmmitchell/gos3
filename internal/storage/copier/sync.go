package copier

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// syncer synchronises one key pair. It holds what both modes share: the
// conditional deletes and metadata updates, version copies and the lock
// checks that guard them.
type syncer struct {
	ctx      context.Context
	src, dst Endpoint
	p        KeyPair
	pol      Policy
	now      time.Time
	w        *writer
	res      *Result
	// fingerprint selects the comparisons of fingerprint mode: a version
	// read from the source is checked against its listing entry by the
	// fields listings report exactly (sameListing).
	fingerprint bool
	// owned: the key may be deleted when the source has none of it.
	owned bool
	// imports caches canImport.
	imports *bool
}

func newSyncer(ctx context.Context, src, dst Endpoint, p KeyPair, pol Policy, res *Result) *syncer {
	return &syncer{
		ctx: ctx, src: src, dst: dst, p: p, pol: pol, now: pol.now(), res: res,
		w:     &writer{b: dst.Backend, bucket: dst.Bucket, pol: pol},
		owned: pol.owns(p.DstKey),
	}
}

// list lists the key on both sides, as the policy's walks would.
func (s *syncer) list() (sv, dv []storage.ObjectInfo, err error) {
	all := s.pol.all()
	if sv, err = listKey(s.ctx, s.src.Backend, s.src.Bucket, s.p.SrcKey, all); err != nil {
		return nil, nil, fmt.Errorf("listing %s on the source %s: %w", s.p.SrcKey, s.src, err)
	}
	if dv, err = listKey(s.ctx, s.dst.Backend, s.dst.Bucket, s.p.DstKey, all); err != nil {
		return nil, nil, fmt.Errorf("listing %s on the target %s: %w", s.p.DstKey, s.dst, err)
	}
	return sv, dv, nil
}

// canImport reports whether the target bucket reports that it imports
// versions (storage.CapsOf). Only a hint: writes still fall back when an
// import is refused.
func (s *syncer) canImport() bool {
	if s.imports == nil {
		ok := false
		if _, imp := s.dst.Backend.(storage.VersionImporter); imp {
			caps, err := storage.CapsOf(s.ctx, s.dst.Backend, s.dst.Bucket)
			ok = err == nil && caps.Import
		}
		s.imports = &ok
	}
	return *s.imports
}

func (s *syncer) conflict(versionID, reason string) {
	s.res.Conflicts = append(s.res.Conflicts, Conflict{Key: s.p.SrcKey, Target: s.p.DstKey, VersionID: versionID, Reason: reason})
}

// LockReason says why version oi must not be deleted or replaced at now:
// it is under a legal hold or unexpired retention ("" when neither, and
// always for delete markers). Code that deletes or replaces versions
// below the S3 layer, which enforces object lock for clients, uses it to
// honour the lock too.
func LockReason(oi storage.ObjectInfo, now time.Time) string { return lockReason(oi, now) }

// lockReason implements LockReason.
func lockReason(oi storage.ObjectInfo, now time.Time) string {
	switch {
	case oi.DeleteMarker:
		return ""
	case oi.LegalHold:
		return "under a legal hold"
	case oi.RetentionMode != "" && oi.RetainUntil.After(now):
		return fmt.Sprintf("under %s retention until %s", oi.RetentionMode, oi.RetainUntil.UTC().Format(time.RFC3339))
	}
	return ""
}

// weakens says why replacing the metadata of version cur with want would
// weaken its object lock: removing a legal hold, removing or shortening
// unexpired retention, or turning COMPLIANCE into GOVERNANCE ("" when it
// would not).
func weakens(cur, want storage.ObjectMeta, now time.Time) string {
	if cur.LegalHold && !want.LegalHold {
		return "under a legal hold, which the update would remove"
	}
	if cur.RetentionMode == "" || !cur.RetainUntil.After(now) {
		return ""
	}
	until := cur.RetainUntil.UTC().Format(time.RFC3339)
	switch {
	case want.RetentionMode == "" || want.RetainUntil.Before(cur.RetainUntil):
		return fmt.Sprintf("under %s retention until %s, which the update would shorten", cur.RetentionMode, until)
	case cur.RetentionMode == "COMPLIANCE" && want.RetentionMode != "COMPLIANCE":
		return fmt.Sprintf("under COMPLIANCE retention until %s, which the update would weaken", until)
	}
	return ""
}

// lockedError is a check's refusal to touch a locked version.
type lockedError struct{ reason string }

func (e *lockedError) Error() string { return "copier: the version is " + e.reason }

func notFound(err error) bool {
	return errors.Is(err, storage.ErrVersionNotFound) || errors.Is(err, storage.ErrObjectNotFound)
}

// deleteVersion deletes target version d of the key, which a listing
// reported, or, with marker, hides it (the key's current version) behind a
// new delete marker. The delete is conditional (storage.ConditionalDeleter):
// it happens only if d is still the version listed and, unless a marker is
// added, is not locked. A locked version is reported as a conflict and
// locked is true. A version that is gone already is not an error.
func (s *syncer) deleteVersion(d storage.ObjectInfo, marker bool) (locked bool, err error) {
	cd, ok := s.dst.Backend.(storage.ConditionalDeleter)
	if !ok {
		s.conflict(d.VersionID, "not deleted: the target cannot delete versions conditionally")
		return true, nil
	}
	id := d.VersionID
	if marker {
		id = ""
	}
	_, err = cd.DeleteObjectIf(s.ctx, s.dst.Bucket, s.p.DstKey, id, func(cur storage.ObjectInfo) error {
		if !sameListing(cur, d) {
			return errChanged
		}
		if !marker {
			if r := lockReason(cur, s.now); r != "" {
				return &lockedError{reason: r}
			}
		}
		return nil
	})
	var le *lockedError
	switch {
	case err == nil:
		s.res.VersionsDeleted++
		return false, nil
	case errors.As(err, &le):
		s.conflict(d.VersionID, "not deleted: it is "+le.reason)
		return true, nil
	case errors.Is(err, errChanged):
		return false, errChanged
	case notFound(err):
		return false, nil
	}
	return false, fmt.Errorf("deleting %s version %s on the target %s: %w", s.p.DstKey, d.VersionID, s.dst, err)
}

// updateMeta replaces the metadata of target version d with want (keeping
// how its data is encrypted), unless d changed since it was listed or the
// update would weaken its object lock (a conflict).
func (s *syncer) updateMeta(d storage.ObjectInfo, want storage.ObjectMeta) error {
	if err := s.w.checkRetention(s.ctx, want); err != nil {
		return err
	}
	fn := func(m *storage.ObjectMeta) error {
		enc := m.Encryption
		*m = CloneMeta(want)
		m.Encryption = enc
		return nil
	}
	var err error
	if cu, ok := s.dst.Backend.(storage.ConditionalMetaUpdater); ok {
		_, err = cu.UpdateObjectMetaIf(s.ctx, s.dst.Bucket, s.p.DstKey, d.VersionID, func(cur storage.ObjectInfo) error {
			if !sameListing(cur, d) {
				return errChanged
			}
			if r := weakens(cur.ObjectMeta, want, s.now); r != "" {
				return &lockedError{reason: r}
			}
			return nil
		}, fn)
	} else {
		if r := weakens(d.ObjectMeta, want, s.now); r != "" {
			err = &lockedError{reason: r}
		} else {
			_, err = s.dst.Backend.UpdateObjectMeta(s.ctx, s.dst.Bucket, s.p.DstKey, d.VersionID, fn)
		}
	}
	var le *lockedError
	switch {
	case err == nil:
		s.res.MetaUpdated++
		return nil
	case errors.As(err, &le):
		s.conflict(d.VersionID, "metadata not updated: it is "+le.reason)
		return nil
	case errors.Is(err, errChanged), notFound(err):
		return errChanged
	}
	return fmt.Errorf("updating the metadata of %s version %s on the target %s: %w", s.p.DstKey, d.VersionID, s.dst, err)
}

// versionLocked says why the target's version id of the key must not be
// replaced ("" when there is none or it is not locked). An import replaces
// the version with its ID wherever it is in the key's history, without
// regard to object lock, so every import that may meet an existing version
// asks first.
func (s *syncer) versionLocked(id string) (string, error) {
	oi, err := s.dst.Backend.GetObjectInfo(s.ctx, s.dst.Bucket, s.p.DstKey, id)
	if notFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s version %s on the target %s: %w", s.p.DstKey, id, s.dst, err)
	}
	return lockReason(oi, s.now), nil
}

// importer reports whether the target may import versions at all (and so
// replace an existing version by ID). Unlike canImport it trusts no
// capability report: lock checks must not be skipped on a hint.
func (s *syncer) importer() bool {
	_, ok := s.dst.Backend.(storage.VersionImporter)
	return ok
}

// consistent reports whether oi, as a read of the source returned it, is
// the version v its listing reported.
func (s *syncer) consistent(oi, v storage.ObjectInfo) bool {
	if s.fingerprint {
		return sameListing(oi, v)
	}
	return SameVersion(oi, v)
}

// copyVersion copies source version v (as listed) to the target key under
// version ID id (when imported). It returns errChanged when v is not
// there any more as listed.
func (s *syncer) copyVersion(v storage.ObjectInfo, id string, mode WriteMode) (Written, error) {
	var (
		wr  Written
		err error
	)
	if v.DeleteMarker {
		wr, err = s.w.write(s.ctx, s.p.DstKey, v, id, nil, mode)
	} else {
		oi, rc, oerr := s.src.Backend.OpenObject(s.ctx, s.src.Bucket, s.p.SrcKey, v.VersionID)
		if oerr != nil {
			if notFound(oerr) {
				return Written{}, errChanged
			}
			return Written{}, fmt.Errorf("reading %s version %s on the source %s: %w", s.p.SrcKey, v.VersionID, s.src, oerr)
		}
		if rc == nil || oi.DeleteMarker || !s.consistent(oi, v) {
			if rc != nil {
				_ = rc.Close()
			}
			return Written{}, errChanged
		}
		// Store what was read: data and metadata of one version.
		wr, err = s.w.write(s.ctx, s.p.DstKey, oi, id, rc, mode)
		_ = rc.Close()
	}
	if err != nil {
		return Written{}, fmt.Errorf("copying %s version %s to %s on the target %s: %w", s.p.SrcKey, v.VersionID, s.p.DstKey, s.dst, err)
	}
	s.res.VersionsCopied++
	s.res.BytesCopied += wr.Bytes
	if !wr.Imported {
		s.res.PlainWrites++
	}
	return wr, nil
}
