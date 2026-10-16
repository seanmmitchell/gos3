package native

import (
	"context"
	"errors"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Metadata updates
//
// UpdateObjectMeta never rewrites the object (a copy onto itself would
// create a remote version and move its LastModified). Each changed field
// goes where it lives (see "Where metadata lives" in meta.go):
//
//   - tags: PutObjectTagging / DeleteObjectTagging of the version when
//     they can be native, else the overlay;
//   - retention and legal hold on buckets with object lock:
//     PutObjectRetention (bypassing GOVERNANCE retention when the S3 layer
//     approved it, storage.GovernanceBypass) and PutObjectLegalHold, so
//     the remote enforces them;
//   - everything else (ACL, contract-only retention, ...): the overlay.
//
// Data, version ID, ETag and LastModified stay as they are.

// UpdateObjectMeta runs fn on a copy of a version's metadata and stores
// the changes (see above).
func (s *Store) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	return s.updateMeta(ctx, bucket, key, versionID, nil, fn)
}

// UpdateObjectMetaIf implements storage.ConditionalMetaUpdater: check sees
// the version under the object's lock, which every gos3 write and
// metadata update of the object takes too.
func (s *Store) UpdateObjectMetaIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if check == nil {
		return storage.ObjectInfo{}, errors.New("native: nil update check")
	}
	return s.updateMeta(ctx, bucket, key, versionID, check, fn)
}

func (s *Store) updateMeta(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if fn == nil {
		return storage.ObjectInfo{}, errors.New("native: nil update function")
	}
	t, err := s.target(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if s.checkKey(key) != nil {
		return storage.ObjectInfo{}, notFound(bucket, key, versionID)
	}
	unlock := s.lockObject(bucket, key)
	defer unlock()
	before := time.Now()
	v, err := s.stat(ctx, t, key, versionID, statFull)
	if err != nil {
		s.pruneMissing(ctx, t, key, versionID, before, true, err)
		return storage.ObjectInfo{}, err
	}
	if v.info.DeleteMarker {
		return storage.ObjectInfo{}, notFound(bucket, key, versionID)
	}
	if check != nil {
		if err := check(v.info); err != nil {
			return storage.ObjectInfo{}, err
		}
	}
	m := cloneMeta(v.info.ObjectMeta)
	if err := fn(&m); err != nil {
		return storage.ObjectInfo{}, err
	}
	m, err = s.applyMeta(ctx, t, key, v, cloneMeta(m), false)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	info := v.info
	info.ObjectMeta = m
	return info, nil
}

// applyMeta changes the stored metadata of version v of key into want (see
// "Metadata updates"). The caller holds the object's lock. Native changes
// are made first; when one fails, the ones before it stay. With
// extendOnly (a new object, whose native retention the remote may have
// set from its default retention a moment after gos3 computed it), native
// retention is only ever extended: a native retention of the wanted mode
// that lasts at least as long is kept. It returns the metadata the version
// has now.
func (s *Store) applyMeta(ctx context.Context, t *target, key string, v version, want storage.ObjectMeta, extendOnly bool) (storage.ObjectMeta, error) {
	cur := v.info.ObjectMeta
	if extendOnly && t.b.ObjectLock && cur.RetentionMode != "" &&
		(want.RetentionMode == "" || want.RetentionMode == cur.RetentionMode && !want.RetainUntil.After(cur.RetainUntil)) {
		want.RetentionMode, want.RetainUntil = cur.RetentionMode, cur.RetainUntil
	}
	patch := diffMeta(cur, want)
	if patch.empty() {
		return want, nil
	}
	rvid := v.attrs.VersionID // "" on an unversioned remote

	if patch.Tags != nil && (v.ov == nil || v.ov.Meta.Tags == nil) {
		tags := *patch.Tags
		if s.tagging(t) && (len(tags) == 0 || nativeTags(tags)) {
			err := s.call(ctx, t, func(c *s3client.Client) error {
				if len(tags) == 0 {
					return c.DeleteObjectTagging(ctx, t.b.Remote, key, rvid)
				}
				return c.PutObjectTagging(ctx, t.b.Remote, key, rvid, tags)
			})
			switch {
			case err == nil:
				if len(tags) > 0 || v.ext == nil || v.ext.Tags == nil {
					patch.Tags = nil // native now; else the empty set hides ext's tags
				}
			case s3client.IsNotImplemented(err):
				s.noNativeTags(t, err.Error())
			default:
				return want, s.remoteErr("PutObjectTagging", t, key, objectOp, err)
			}
		}
	}

	if t.b.ObjectLock {
		if patch.RetentionMode != nil || patch.RetainUntil != nil {
			if err := s.checkCompliance(t, want.RetentionMode); err != nil {
				return want, err
			}
			r := s3client.Retention{Mode: want.RetentionMode, RetainUntil: want.RetainUntil}
			if r.Mode == "" || r.RetainUntil.IsZero() {
				r = s3client.Retention{}
			}
			err := s.call(ctx, t, func(c *s3client.Client) error {
				return c.PutObjectRetention(ctx, t.b.Remote, key, rvid, r, storage.GovernanceBypass(ctx))
			})
			if err != nil {
				return want, s.remoteErr("PutObjectRetention", t, key, objectOp, err)
			}
			ru := r.RetainUntil
			if !r.RetainUntil.IsZero() && (!ceilMillis(ru).Equal(ru) || hasPrecise(v)) {
				patch.RetainPrecise = &ru
			}
		}
		if patch.LegalHold != nil {
			err := s.call(ctx, t, func(c *s3client.Client) error {
				return c.PutObjectLegalHold(ctx, t.b.Remote, key, rvid, want.LegalHold)
			})
			if err != nil {
				return want, s.remoteErr("PutObjectLegalHold", t, key, objectOp, err)
			}
		}
		// The remote holds them.
		patch.RetentionMode, patch.RetainUntil, patch.LegalHold = nil, nil, nil
	}

	return want, s.putOverlay(ctx, overlayID{t.bucket, key, v.info.VersionID}, v.attrs.ETag, v.attrs.LastModified, patch)
}

// hasPrecise reports whether v's retain-until date was refined by a
// precise date (which must then be replaced, not left to match a new
// date by chance).
func hasPrecise(v version) bool {
	return v.ext != nil && v.ext.RetainPrecise != nil || v.ov != nil && v.ov.Meta.RetainPrecise != nil
}
