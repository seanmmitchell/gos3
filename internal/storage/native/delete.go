package native

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Deletes
//
// Without a version ID, a delete removes the object of an unversioned
// bucket and adds a delete marker in a versioned one; the marker's version
// ID comes from the answer's headers (or, when the service sends none, a
// HEAD). A delete by version ID first looks the version up (HEAD, also
// telling objects from delete markers): AWS answers 204 for version IDs it
// does not know, so the DELETE alone cannot report ErrVersionNotFound. The
// DELETE then carries x-amz-bypass-governance-retention only when the S3
// layer approved a bypass (storage.GovernanceBypass): the remote enforces
// retention and legal holds itself, and a refusal is ErrRemoteDenied.
//
// The conditional deletes (DeleteObjectIf, DeleteObjectChecked,
// DeleteVersionIf) look up,
// check and delete under the object's lock: atomic against other users of
// this Store. s3client has no conditional DELETE, so a client writing the
// object directly on the remote between the check and the delete is not
// detected.

// DeleteObject deletes per S3 semantics (see storage.Backend).
func (s *Store) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	t, err := s.target(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if versionID != "" {
		return s.deleteVersion(ctx, t, key, versionID, nil)
	}
	if err := s.checkKey(key); err != nil {
		if !t.versioned() {
			return storage.DeleteResult{}, nil // nothing can be stored there
		}
		return storage.DeleteResult{}, err
	}
	unlock := s.lockObject(bucket, key)
	defer unlock()
	return s.deleteLatest(ctx, t, key)
}

// DeleteObjectIf implements storage.ConditionalDeleter: the version is
// read (HEAD) and checked, then deleted, under the object's lock.
func (s *Store) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("native: nil delete check")
	}
	t, err := s.target(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if versionID != "" {
		return s.deleteVersion(ctx, t, key, versionID, check)
	}
	if s.checkKey(key) != nil {
		return storage.DeleteResult{}, notFound(bucket, key, "")
	}
	unlock := s.lockObject(bucket, key)
	defer unlock()
	v, err := s.stat(ctx, t, key, "", statFull)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if err := check(v.info); err != nil {
		return storage.DeleteResult{}, err
	}
	return s.deleteLatest(ctx, t, key)
}

// DeleteObjectChecked implements storage.CheckedDeleter: DeleteObjectIf,
// except that a key without versions is checked with found = false and,
// when the check allows it, deleted (a delete marker in a versioned
// bucket).
func (s *Store) DeleteObjectChecked(ctx context.Context, bucket, key, versionID string, check func(info storage.ObjectInfo, found bool) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("native: nil delete check")
	}
	t, err := s.target(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if versionID != "" {
		return s.deleteVersion(ctx, t, key, versionID, func(info storage.ObjectInfo) error { return check(info, true) })
	}
	if err := s.checkKey(key); err != nil {
		if cerr := check(storage.ObjectInfo{}, false); cerr != nil {
			return storage.DeleteResult{}, cerr
		}
		if !t.versioned() {
			return storage.DeleteResult{}, nil
		}
		return storage.DeleteResult{}, err
	}
	unlock := s.lockObject(bucket, key)
	defer unlock()
	v, err := s.stat(ctx, t, key, "", statFull)
	found := err == nil
	if err != nil && !errors.Is(err, storage.ErrObjectNotFound) {
		return storage.DeleteResult{}, err
	}
	if err := check(v.info, found); err != nil {
		return storage.DeleteResult{}, err
	}
	return s.deleteLatest(ctx, t, key)
}

// deleteLatest deletes key without a version ID. The caller holds the
// object's lock.
func (s *Store) deleteLatest(ctx context.Context, t *target, key string) (storage.DeleteResult, error) {
	nullID := overlayID{t.bucket, key, storage.NullVersionID}
	var hidden *overlay
	if t.nullWrites() {
		// The null version is removed or replaced by a null marker: its
		// overlay goes once the remote acknowledged the delete.
		hidden = s.hideOverlay(nullID)
	}
	var res s3client.DeleteResult
	err := s.call(ctx, t, func(c *s3client.Client) error {
		var err error
		res, err = c.DeleteObjectWith(ctx, t.b.Remote, key, s3client.DeleteOptions{})
		return err
	})
	if err != nil {
		s.restoreOverlay(nullID, hidden)
		return storage.DeleteResult{}, s.remoteErr("DeleteObject", t, key, objectOp, err)
	}
	s.forgetOverlay(ctx, nullID, hidden)
	if !t.versioned() {
		if res.DeleteMarker {
			s.drift(t.bucket, "the remote bucket is versioned")
		}
		return storage.DeleteResult{}, nil
	}
	if res.VersionID == "" {
		// The service did not report the marker it created.
		v, err := s.stat(ctx, t, key, "", statFull)
		if err != nil || !v.info.DeleteMarker {
			s.drift(t.bucket, "the remote bucket created no delete marker")
			return storage.DeleteResult{}, nil
		}
		return storage.DeleteResult{VersionID: v.info.VersionID, DeleteMarker: true}, nil
	}
	// A delete without version ID in a versioned bucket creates a delete
	// marker (some services omit x-amz-delete-marker).
	return storage.DeleteResult{VersionID: res.VersionID, DeleteMarker: true}, nil
}

// DeleteVersionIf implements storage.NewerVersionsDeleter. Under the
// object's lock, the key's versions are listed newest first down to vid
// (the remote cannot list the versions newer than a given one any other
// way), keeping the last limit seen; then the version is read, checked
// with them and deleted as by DeleteObjectIf. A version the listing does
// not show is ErrVersionNotFound.
func (s *Store) DeleteVersionIf(ctx context.Context, bucket, key, vid string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("native: nil delete check")
	}
	t, err := s.target(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if vid == "" || s.checkKey(key) != nil {
		return storage.DeleteResult{}, notFound(t.bucket, key, vid)
	}
	unlock := s.lockObject(t.bucket, key)
	defer unlock()
	limit = max(limit, 1)
	var (
		win   []storage.ObjectInfo // the last limit versions before vid, newest first
		found bool
	)
	// The walk may prune the key's overlays; it must not take the lock
	// held here again.
	err = s.walkVersionInfos(ctx, t, key, "", "", key, func(o storage.ObjectInfo) error {
		if o.Key != key {
			return storage.ErrStopWalk // keys extending key sort after it
		}
		if o.VersionID == vid {
			found = true
			return storage.ErrStopWalk
		}
		if len(win) == limit {
			win = append(win[:0], win[1:]...)
		}
		win = append(win, o)
		return nil
	})
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if !found {
		return storage.DeleteResult{}, notFound(t.bucket, key, vid)
	}
	slices.Reverse(win)
	return s.deleteVersionLocked(ctx, t, key, vid, func(info storage.ObjectInfo) error { return check(info, win) })
}

// deleteVersion permanently deletes version vid of key after check (nil =
// none) accepted it.
func (s *Store) deleteVersion(ctx context.Context, t *target, key, vid string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if s.checkKey(key) != nil {
		return storage.DeleteResult{}, notFound(t.bucket, key, vid)
	}
	unlock := s.lockObject(t.bucket, key)
	defer unlock()
	return s.deleteVersionLocked(ctx, t, key, vid, check)
}

// deleteVersionLocked is deleteVersion for a caller holding the object's
// lock.
func (s *Store) deleteVersionLocked(ctx context.Context, t *target, key, vid string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	mode := statQuick
	if check != nil {
		mode = statFull
	}
	before := time.Now()
	v, err := s.stat(ctx, t, key, vid, mode)
	if err != nil {
		s.pruneMissing(ctx, t, key, vid, before, true, err)
		return storage.DeleteResult{}, err
	}
	if check != nil {
		if err := check(v.info); err != nil {
			return storage.DeleteResult{}, err
		}
	}
	opts := s3client.DeleteOptions{VersionID: vid, BypassGovernance: storage.GovernanceBypass(ctx)}
	if !v.info.DeleteMarker && v.attrs.VersionID == "" {
		// The current object of an unversioned remote: deleting it
		// without a version ID works on services without versioning.
		opts.VersionID = ""
	}
	err = s.call(ctx, t, func(c *s3client.Client) error {
		_, err := c.DeleteObjectWith(ctx, t.b.Remote, key, opts)
		return err
	})
	if err != nil {
		if opts.VersionID != "" && missingObject(err) {
			s.dropOverlay(ctx, overlayID{t.bucket, key, v.info.VersionID})
			return storage.DeleteResult{}, notFound(t.bucket, key, vid)
		}
		if opts.VersionID != "" && missingVersion(err) {
			return storage.DeleteResult{}, notFound(t.bucket, key, vid)
		}
		return storage.DeleteResult{}, s.remoteErr("DeleteObject", t, key, objectOp, err)
	}
	s.dropOverlay(ctx, overlayID{t.bucket, key, v.info.VersionID})
	return storage.DeleteResult{VersionID: vid, DeleteMarker: v.info.DeleteMarker}, nil
}
