package s3account

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
	"github.com/seanmmitchell/gos3/internal/storage/native"
)

// Dispatch
//
//	method                  wrapped bucket               native bucket
//	ListBuckets             container info (registry's   registry info
//	                        cache while not loaded)
//	CreateBucket            container, then record       claim, native.AttachBucket, then record
//	GetBucket               container                    registry info
//	DeleteBucket            container (empty), records   CheckEmpty, DetachBucket (removing a
//	                                                     remote bucket gos3 created), records
//	SetBucketVersioning /   container (+ cached info)    remote (native store), then record
//	SetBucketObjectLock
//	bucket configurations   container                    records (object lock: remote first)
//	objects, uploads        container                    native store (BeforePublish sees the
//	                                                     records' configurations)
//	ReleaseBucket           storage.ErrNotSupported      DetachBucket(false), records
//
// Every result's BucketInfo carries the bucket's Kind and Location.

// target is a bucket resolved for one operation: c for a wrapped bucket,
// nat for a native one.
type target struct {
	name string
	rec  bucketRecord
	c    *container
	nat  NativeStore
}

func (t *target) native() bool { return t.rec.Kind == storage.BucketKindNative }

// lookup resolves bucket. A wrapped bucket whose container cannot be used
// and a native bucket whose remote bucket is claimed elsewhere answer
// storage.ErrBucketUnavailable.
func (b *Backend) lookup(ctx context.Context, bucket string) (target, error) {
	t, unusable, err := b.lookupAny(ctx, bucket)
	if err == nil && unusable != nil {
		err = unusable
	}
	return t, err
}

// lookupAny is lookup that reports an unusable bucket's error separately.
func (b *Backend) lookupAny(ctx context.Context, bucket string) (t target, unusable, err error) {
	if err := b.serving(ctx); err != nil {
		return target{}, nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	e := b.reg.buckets[bucket]
	if e == nil {
		return target{}, nil, fmt.Errorf("%w: %q", storage.ErrBucketNotFound, bucket)
	}
	t = target{name: bucket, rec: e.bucketRecord}
	if t.native() {
		t.nat = b.nat
		if e.claimErr != nil {
			unusable = fmt.Errorf("s3account %q: bucket %q: %w: %w", b.name, bucket, storage.ErrBucketUnavailable, e.claimErr)
		}
		return t, unusable, nil
	}
	t.c = b.containers[e.Container]
	switch {
	case t.c == nil:
		unusable = fmt.Errorf("s3account %q: bucket %q: container %q is not registered: %w", b.name, bucket, e.Container, storage.ErrBucketUnavailable)
	case t.c.store == nil:
		unusable = t.c.unusable()
	}
	return t, unusable, nil
}

// withKind sets the output-only fields of a bucket's info.
func (t *target) withKind(info storage.BucketInfo) storage.BucketInfo {
	info.Kind = t.rec.Kind
	if t.native() {
		info.Location = t.rec.nativeLocation()
	} else if t.c != nil {
		info.Location = t.c.locationString()
	}
	return info
}

// --- buckets ---

// ListBuckets returns every bucket of the registry, sorted by name.
func (b *Backend) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	if err := b.serving(ctx); err != nil {
		return nil, err
	}
	b.mu.RLock()
	targets := make([]target, 0, len(b.reg.buckets))
	used := map[string]*container{}
	for name, e := range b.reg.buckets {
		t := target{name: name, rec: e.bucketRecord}
		if !t.native() {
			t.c = b.containers[e.Container]
			if t.c != nil {
				used[t.c.id] = t.c
			}
		}
		targets = append(targets, t)
	}
	b.mu.RUnlock()
	live := map[string]map[string]storage.BucketInfo{}
	for id, c := range used {
		if !c.loaded() {
			continue
		}
		list, err := c.store.ListBuckets(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue // the registry's cached info
		}
		m := make(map[string]storage.BucketInfo, len(list))
		for _, info := range list {
			m[info.Name] = info
		}
		live[id] = m
	}
	out := make([]storage.BucketInfo, 0, len(targets))
	for _, t := range targets {
		info := t.rec.Info
		if t.c != nil {
			if li, ok := live[t.c.id][t.name]; ok {
				info = li
			}
		}
		info.Name = t.name
		out = append(out, t.withKind(info))
	}
	slices.SortFunc(out, func(a, c storage.BucketInfo) int { return strings.Compare(a.Name, c.Name) })
	return out, nil
}

// validBucketName reports whether name can be a record key.
func validBucketName(name string) error {
	if name == "" || strings.Contains(name, "/") || !utf8.ValidString(name) {
		return fmt.Errorf("%w: bucket %q", storage.ErrInvalidName, name)
	}
	return nil
}

// CreateBucket creates a bucket of the kind and at the location opts ask
// for (see planCreate): a wrapped bucket in its container, or a native
// bucket bound to a remote bucket that is created (or, with Target.Adopt,
// adopted). The record is written last, so an interrupted creation leaves
// a bucket the next reconciliation adopts (wrapped) or an unused remote
// bucket (native).
func (b *Backend) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	if err := b.serving(ctx); err != nil {
		return err
	}
	if err := validBucketName(name); err != nil {
		return err
	}
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	b.mu.RLock()
	_, exists := b.reg.buckets[name]
	p, err := b.planCreate(opts)
	var c *container
	if err == nil && p.kind == storage.BucketKindWrapped {
		c = b.containers[p.container]
	}
	nat := b.nat
	b.mu.RUnlock()
	switch {
	case exists:
		return fmt.Errorf("%w: %q", storage.ErrBucketExists, name)
	case err != nil:
		return err
	case p.kind == storage.BucketKindWrapped:
		return b.createWrapped(ctx, name, opts, p.container, c)
	}
	return b.createNative(ctx, name, opts, p, nat)
}

// createWrapped creates a wrapped bucket in container c (adminMu held).
func (b *Backend) createWrapped(ctx context.Context, name string, opts storage.CreateBucketOptions, id string, c *container) error {
	switch {
	case c == nil:
		return fmt.Errorf("%w: s3 backing %q has no container %q", storage.ErrInvalidTarget, b.name, id)
	case c.store == nil:
		return fmt.Errorf("%w: container %s (%s) cannot be used: %w", storage.ErrInvalidTarget, c.id, c.locationString(), c.failErr)
	}
	so := opts
	so.Target, so.Import = nil, false
	if err := c.store.CreateBucket(ctx, name, so); err != nil {
		return c.classify(err)
	}
	if err := b.step("create-wrapped:stored"); err != nil {
		return err
	}
	info, err := c.store.GetBucket(ctx, name)
	if err != nil {
		info = storage.BucketInfo{Name: name, CreatedAt: time.Now().UTC(), Region: opts.Region,
			ObjectLockEnabled: opts.ObjectLockEnabled, Owner: opts.Owner, Backing: opts.Backing}
		if opts.ObjectLockEnabled {
			info.Versioning = storage.VersioningEnabled
		}
	}
	rec := bucketRecord{ID: newIncarnation(), Kind: storage.BucketKindWrapped, Container: c.id, Info: info, Added: time.Now().UTC()}
	if err := b.putBucket(ctx, name, rec, nil); err != nil {
		// The record may have been written although the write failed:
		// remove both (a leftover of either is repaired at the next load).
		dctx := context.WithoutCancel(ctx)
		if derr := c.store.DeleteBucket(dctx, name); derr != nil {
			b.log.Warn("s3account: removing a bucket whose record could not be written failed; it is adopted at the next start",
				"backing", b.name, "bucket", name, "container", c.id, "err", derr)
		}
		_ = b.records.DeleteRecords(dctx, bucketKey(name))
		return err
	}
	return nil
}

// createNative creates a native bucket (adminMu held).
func (b *Backend) createNative(ctx context.Context, name string, opts storage.CreateBucketOptions, p createPlan, nat NativeStore) error {
	if nat == nil {
		return b.unavailable()
	}
	remote := p.remote
	if remote == "" {
		remote = name
	}
	release, err := b.claim(Location{Bucket: remote}, RoleNative, name)
	if err != nil {
		return err
	}
	intent := native.Create
	if p.adopt {
		intent = native.Adopt
	}
	b.setPending(name, native.Bucket{Binding: native.Binding{Remote: remote, Region: p.region, Owned: !p.adopt}})
	defer b.clearPending(name)
	// Nothing of an earlier bucket of this name carries over: neither
	// configuration records an interrupted deletion left, nor the native
	// store's state for the name (uploads, overlays).
	if err := b.purgeConfigs(ctx, name); err != nil {
		release()
		return err
	}
	if err := nat.DetachBucket(ctx, name, false); err != nil {
		release()
		return fmt.Errorf("s3account %q: clear the native state of an earlier bucket %q: %w", b.name, name, err)
	}
	rs, err := nat.AttachBucket(ctx, native.AttachRequest{
		Bucket: name, Remote: remote, Region: p.region, Intent: intent, ObjectLock: opts.ObjectLockEnabled,
	})
	if err != nil {
		release()
		return err
	}
	if rs.Remote == "" {
		rs.Remote = remote
	}
	if err := b.step("create-native:attached"); err != nil {
		release()
		return err
	}
	now := time.Now().UTC()
	rec := bucketRecord{
		ID: newIncarnation(), Kind: storage.BucketKindNative, Remote: rs.Remote, RemoteRegion: rs.Region, Created: intent == native.Create,
		Info:  storage.BucketInfo{Name: name, CreatedAt: now, Region: opts.Region, Owner: opts.Owner, Backing: opts.Backing},
		Added: now,
	}
	rec = applyRemoteState(rec, rs)
	b.setPending(name, native.Bucket{Binding: rec.binding(), Versioning: rec.Info.Versioning, ObjectLock: rec.Info.ObjectLockEnabled})
	undo := func(cause error) error {
		dctx := context.WithoutCancel(ctx)
		if err := nat.DetachBucket(dctx, name, rec.Created); err != nil {
			b.log.Warn("s3account: detach a native bucket whose creation failed", "backing", b.name, "bucket", name, "err", err)
		}
		// The records may have been written although a write failed.
		for _, p := range []string{bucketKey(name), configPrefix(name)} {
			if err := b.records.DeleteRecords(dctx, p); err != nil {
				b.log.Warn("s3account: delete the records of a failed creation", "backing", b.name, "bucket", name, "err", err)
			}
		}
		release()
		return cause
	}
	lockDoc := rs.LockConfig
	if opts.ObjectLockEnabled && !rec.Info.ObjectLockEnabled {
		if err := nat.EnableObjectLock(ctx, name); err != nil {
			return undo(err)
		}
		rec.Info.ObjectLockEnabled, rec.Info.Versioning = true, storage.VersioningEnabled
	}
	if lockDoc != nil {
		if err := b.records.PutRecord(ctx, configKey(name, storage.ConfigObjectLock), lockDoc); err != nil {
			return undo(fmt.Errorf("s3account %q: write the object lock configuration of bucket %q: %w", b.name, name, err))
		}
	}
	if err := b.putBucket(ctx, name, rec, release); err != nil {
		return undo(err)
	}
	if lockDoc != nil {
		b.mu.Lock()
		b.reg.configs[name] = map[string][]byte{storage.ConfigObjectLock: bytes.Clone(lockDoc)}
		b.mu.Unlock()
	}
	return nil
}

func (b *Backend) setPending(name string, nb native.Bucket) {
	b.mu.Lock()
	b.pending[name] = nb
	b.mu.Unlock()
}

func (b *Backend) clearPending(name string) {
	b.mu.Lock()
	delete(b.pending, name)
	b.mu.Unlock()
}

// GetBucket returns a bucket's info.
func (b *Backend) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	return b.bucketInfo(ctx, name, false)
}

// GetBucketIdentity returns the bucket state used to bind a copy job. For a
// native bucket, CreatedAt comes from a fresh remote ListBuckets response,
// rather than the local time at which gos3 attached the remote bucket.
// Without a verifiable remote creation date, a job must not write to it.
func (b *Backend) GetBucketIdentity(ctx context.Context, name string) (storage.BucketInfo, error) {
	return b.bucketInfo(ctx, name, true)
}

func (b *Backend) bucketInfo(ctx context.Context, name string, verifyRemote bool) (storage.BucketInfo, error) {
	t, err := b.lookup(ctx, name)
	if err != nil {
		return storage.BucketInfo{}, err
	}
	if t.native() {
		info := t.rec.Info
		info.Name = name
		if verifyRemote {
			created, err := b.remoteCreationDate(ctx, t.rec.Remote)
			if err != nil {
				return storage.BucketInfo{}, err
			}
			info.CreatedAt = created
		}
		return t.withKind(info), nil
	}
	info, err := t.c.store.GetBucket(ctx, name)
	if err != nil {
		return storage.BucketInfo{}, t.c.classify(err)
	}
	return t.withKind(info), nil
}

func (b *Backend) remoteCreationDate(ctx context.Context, remote string) (time.Time, error) {
	found := errors.New("remote bucket found")
	var created time.Time
	matched := false
	err := b.client().WalkBuckets(ctx, remote, func(entry s3client.BucketEntry) error {
		if entry.Name != remote {
			return nil
		}
		matched = true
		created = entry.CreationDate
		return found
	})
	if err != nil && !errors.Is(err, found) {
		return time.Time{}, b.remoteError("verify the native bucket's creation date", err)
	}
	if !matched {
		return time.Time{}, fmt.Errorf("s3account %q: remote bucket %q is missing: %w", b.name, remote, storage.ErrBucketNotFound)
	}
	if created.IsZero() {
		return time.Time{}, fmt.Errorf("s3account %q: remote bucket %q has no verifiable creation date: %w", b.name, remote, storage.ErrNotSupported)
	}
	return created, nil
}

// DeleteBucket deletes an empty bucket. A native bucket's remote bucket is
// deleted too when gos3 created it; an adopted one is left alone.
func (b *Backend) DeleteBucket(ctx context.Context, name string) error {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	t, err := b.lookup(ctx, name)
	if err != nil {
		return err
	}
	if t.native() {
		if err := t.nat.CheckEmpty(ctx, name); err != nil {
			return err
		}
		if err := t.nat.DetachBucket(ctx, name, t.rec.Created); err != nil {
			return err
		}
		if err := b.step("delete-native:detached"); err != nil {
			return err
		}
		return b.dropBucket(context.WithoutCancel(ctx), name)
	}
	err = t.c.store.DeleteBucket(ctx, name)
	if err == nil {
		if err := b.step("delete-wrapped:deleted"); err != nil {
			return err
		}
	}
	switch {
	case errors.Is(err, storage.ErrBucketNotFound) && t.c.loaded():
		// The container lost the bucket: forget it (as reconciling does).
		b.log.Warn("s3account: a wrapped bucket was missing from its container; its record is removed",
			"backing", b.name, "bucket", name, "container", t.c.id)
	case err != nil:
		return t.c.classify(err)
	}
	return b.dropBucket(context.WithoutCancel(ctx), name)
}

// ReleaseBucket implements storage.BucketReleaser: a native bucket is
// forgotten without touching its remote bucket or objects (its uploads in
// progress are aborted); wrapped buckets cannot be released
// (storage.ErrNotSupported: their data would be orphaned).
func (b *Backend) ReleaseBucket(ctx context.Context, name string) error {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	t, _, err := b.lookupAny(ctx, name)
	if err != nil {
		return err
	}
	if !t.native() {
		return fmt.Errorf("%w: bucket %q is a wrapped bucket, stored in gos3's format: releasing it would orphan its data; "+
			"move or delete it instead", storage.ErrNotSupported, name)
	}
	if err := t.nat.DetachBucket(ctx, name, false); err != nil {
		return err
	}
	return b.dropBucket(context.WithoutCancel(ctx), name)
}

// SetBucketVersioning changes a bucket's versioning (on a native bucket:
// the remote bucket's, then the record).
func (b *Backend) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	t, err := b.lookup(ctx, name)
	if err != nil {
		return err
	}
	if !t.native() {
		if err := t.c.store.SetBucketVersioning(ctx, name, status); err != nil {
			return t.c.classify(err)
		}
		b.updateCachedInfo(ctx, t)
		return nil
	}
	if t, err = b.relockNative(ctx, t); err != nil {
		return err
	}
	defer b.adminMu.Unlock()
	if status == storage.VersioningUnversioned {
		if t.rec.Info.Versioning != storage.VersioningUnversioned {
			return errors.New("s3account: a versioned bucket cannot become unversioned")
		}
		return nil
	}
	if err := t.nat.SetVersioning(ctx, name, status); err != nil {
		return err
	}
	return b.updateNative(ctx, name, func(r *bucketRecord) { r.Info.Versioning = status })
}

// SetBucketObjectLock sets a bucket's object lock flag (enabling it on a
// native bucket enables it on the remote bucket first).
func (b *Backend) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	t, err := b.lookup(ctx, name)
	if err != nil {
		return err
	}
	if !t.native() {
		if err := t.c.store.SetBucketObjectLock(ctx, name, enabled); err != nil {
			return t.c.classify(err)
		}
		b.updateCachedInfo(ctx, t)
		return nil
	}
	if t, err = b.relockNative(ctx, t); err != nil {
		return err
	}
	defer b.adminMu.Unlock()
	if enabled {
		if err := t.nat.EnableObjectLock(ctx, name); err != nil {
			return err
		}
	}
	return b.updateNative(ctx, name, func(r *bucketRecord) { r.Info.ObjectLockEnabled = enabled })
}

// relockNative takes adminMu for a change of native bucket t, looked up
// before, and checks that the bucket is still that bucket: not deleted
// meanwhile, nor deleted and created again under the same name (whose
// configuration and settings must not receive the change). On success
// adminMu stays held for the caller to unlock, and the bucket is returned
// as it is now.
func (b *Backend) relockNative(ctx context.Context, t target) (target, error) {
	b.adminMu.Lock()
	cur, err := b.lookup(ctx, t.name)
	if err == nil && !sameIncarnation(cur.rec, t.rec) {
		err = fmt.Errorf("%w: %q (deleted meanwhile)", storage.ErrBucketNotFound, t.name)
	}
	if err != nil {
		b.adminMu.Unlock()
		return target{}, err
	}
	return cur, nil
}

// updateNative rewrites a native bucket's record with fn applied (adminMu
// held).
func (b *Backend) updateNative(ctx context.Context, name string, fn func(*bucketRecord)) error {
	b.mu.RLock()
	e := b.reg.buckets[name]
	var rec bucketRecord
	if e != nil {
		rec = e.bucketRecord
	}
	b.mu.RUnlock()
	if e == nil || rec.Kind != storage.BucketKindNative {
		return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	}
	fn(&rec)
	return b.putBucket(ctx, name, rec, nil)
}

// updateCachedInfo refreshes the registry's cached info of a wrapped
// bucket after a change (best effort: the cache only serves listings while
// the container is not loaded).
func (b *Backend) updateCachedInfo(ctx context.Context, t target) {
	info, err := t.c.store.GetBucket(ctx, t.name)
	if err != nil {
		return
	}
	info = storedInfo(info)
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	b.mu.RLock()
	e := b.reg.buckets[t.name]
	var rec bucketRecord
	if e != nil {
		rec = e.bucketRecord
	}
	b.mu.RUnlock()
	if e == nil || rec.Kind != storage.BucketKindWrapped || rec.Container != t.c.id || rec.Info == info {
		return
	}
	rec.Info = info
	if err := b.putBucket(context.WithoutCancel(ctx), t.name, rec, nil); err != nil {
		b.log.Warn("s3account: update the cached info of a wrapped bucket", "backing", b.name, "bucket", t.name, "err", err)
	}
}

// --- bucket configurations ---

// GetBucketConfig returns a configuration document.
func (b *Backend) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if !t.native() {
		data, err := t.c.store.GetBucketConfig(ctx, bucket, key)
		return data, t.c.classify(err)
	}
	data := b.nativeConfig(bucket, key)
	if data == nil {
		return nil, fmt.Errorf("%w: %s of %q", storage.ErrConfigNotFound, key, bucket)
	}
	return bytes.Clone(data), nil
}

// ViewBucketConfig implements storage.ConfigViewer.
func (b *Backend) ViewBucketConfig(ctx context.Context, bucket, key string, fn func(data []byte) error) error {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return err
	}
	if !t.native() {
		return t.c.walkErr(t.c.store.ViewBucketConfig(ctx, bucket, key, markCallback(fn)))
	}
	data := b.nativeConfig(bucket, key)
	if data == nil {
		return fmt.Errorf("%w: %s of %q", storage.ErrConfigNotFound, key, bucket)
	}
	return fn(data)
}

// PutBucketConfig stores a configuration document. A native bucket's
// object lock configuration is mirrored to the remote bucket first.
func (b *Backend) PutBucketConfig(ctx context.Context, bucket, key string, data []byte) error {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return err
	}
	if !t.native() {
		return t.c.classify(t.c.store.PutBucketConfig(ctx, bucket, key, data))
	}
	if t, err = b.relockNative(ctx, t); err != nil {
		return err
	}
	defer b.adminMu.Unlock()
	if key == storage.ConfigObjectLock {
		if err := t.nat.PutObjectLockConfig(ctx, bucket, data); err != nil {
			return err
		}
	}
	return b.putConfig(ctx, bucket, key, data)
}

// DeleteBucketConfig deletes a configuration document (idempotent).
func (b *Backend) DeleteBucketConfig(ctx context.Context, bucket, key string) error {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return err
	}
	if !t.native() {
		return t.c.classify(t.c.store.DeleteBucketConfig(ctx, bucket, key))
	}
	if _, err := b.relockNative(ctx, t); err != nil {
		return err
	}
	defer b.adminMu.Unlock()
	if b.nativeConfig(bucket, key) == nil {
		return nil
	}
	return b.deleteConfig(ctx, bucket, key)
}

// --- objects ---

func (b *Backend) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if t.native() {
		w, err := t.nat.NewObjectWriter(ctx, bucket, key)
		if err != nil {
			return nil, err
		}
		return &nativeWriter{ObjectWriter: w, b: b, bucket: bucket}, nil
	}
	w, err := t.c.store.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		return nil, t.c.classify(err)
	}
	return t.c.wrapObjectWriter(w), nil
}

func (b *Backend) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if t.native() {
		return t.nat.GetObjectInfo(ctx, bucket, key, versionID)
	}
	info, err := t.c.store.GetObjectInfo(ctx, bucket, key, versionID)
	return info, t.c.classify(err)
}

func (b *Backend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	if t.native() {
		return t.nat.OpenObject(ctx, bucket, key, versionID)
	}
	info, r, err := t.c.store.OpenObject(ctx, bucket, key, versionID)
	if err != nil {
		return info, nil, t.c.classify(err)
	}
	return info, t.c.wrapReader(r), nil
}

func (b *Backend) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if t.native() {
		return t.nat.UpdateObjectMeta(ctx, bucket, key, versionID, fn)
	}
	info, err := t.c.store.UpdateObjectMeta(ctx, bucket, key, versionID, markCallback(fn))
	return info, t.c.walkErr(err)
}

// UpdateObjectMetaIf implements storage.ConditionalMetaUpdater.
func (b *Backend) UpdateObjectMetaIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if t.native() {
		return t.nat.UpdateObjectMetaIf(ctx, bucket, key, versionID, check, fn)
	}
	info, err := t.c.store.UpdateObjectMetaIf(ctx, bucket, key, versionID, markCallback(check), markCallback(fn))
	return info, t.c.walkErr(err)
}

func (b *Backend) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if t.native() {
		return t.nat.DeleteObject(ctx, bucket, key, versionID)
	}
	res, err := t.c.store.DeleteObject(ctx, bucket, key, versionID)
	return res, t.c.classify(err)
}

// DeleteObjectIf implements storage.ConditionalDeleter.
func (b *Backend) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if t.native() {
		return t.nat.DeleteObjectIf(ctx, bucket, key, versionID, check)
	}
	res, err := t.c.store.DeleteObjectIf(ctx, bucket, key, versionID, markCallback(check))
	return res, t.c.walkErr(err)
}

// DeleteVersionIf implements storage.NewerVersionsDeleter.
func (b *Backend) DeleteVersionIf(ctx context.Context, bucket, key, versionID string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if t.native() {
		return t.nat.DeleteVersionIf(ctx, bucket, key, versionID, limit, check)
	}
	res, err := t.c.store.DeleteVersionIf(ctx, bucket, key, versionID, limit, func(v storage.ObjectInfo, newer []storage.ObjectInfo) error {
		if err := check(v, newer); err != nil {
			return &callbackErr{err}
		}
		return nil
	})
	return res, t.c.walkErr(err)
}

// DeleteObjectChecked implements storage.CheckedDeleter.
func (b *Backend) DeleteObjectChecked(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo, bool) error) (storage.DeleteResult, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if t.native() {
		return t.nat.DeleteObjectChecked(ctx, bucket, key, versionID, check)
	}
	res, err := t.c.store.DeleteObjectChecked(ctx, bucket, key, versionID, func(info storage.ObjectInfo, found bool) error {
		if err := check(info, found); err != nil {
			return &callbackErr{err}
		}
		return nil
	})
	return res, t.c.walkErr(err)
}

func (b *Backend) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return err
	}
	if t.native() {
		return t.nat.WalkObjects(ctx, bucket, prefix, after, fn)
	}
	return t.c.walkErr(t.c.store.WalkObjects(ctx, bucket, prefix, after, markCallback(fn)))
}

func (b *Backend) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return err
	}
	if t.native() {
		return t.nat.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn)
	}
	return t.c.walkErr(t.c.store.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, markCallback(fn)))
}

// --- multipart uploads ---

func (b *Backend) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	if t.native() {
		return t.nat.CreateMultipartUpload(ctx, bucket, key, meta, initiator)
	}
	u, err := t.c.store.CreateMultipartUpload(ctx, bucket, key, meta, initiator)
	return u, t.c.classify(err)
}

func (b *Backend) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	if t.native() {
		return t.nat.GetMultipartUpload(ctx, bucket, key, uploadID)
	}
	u, err := t.c.store.GetMultipartUpload(ctx, bucket, key, uploadID)
	return u, t.c.classify(err)
}

func (b *Backend) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if t.native() {
		return t.nat.NewPartWriter(ctx, bucket, key, uploadID, partNumber)
	}
	w, err := t.c.store.NewPartWriter(ctx, bucket, key, uploadID, partNumber)
	if err != nil {
		return nil, t.c.classify(err)
	}
	return t.c.wrapPartWriter(w), nil
}

func (b *Backend) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if t.native() {
		return t.nat.ListParts(ctx, bucket, key, uploadID)
	}
	parts, err := t.c.store.ListParts(ctx, bucket, key, uploadID)
	return parts, t.c.classify(err)
}

func (b *Backend) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if t.native() {
		return t.nat.ListMultipartUploads(ctx, bucket)
	}
	ups, err := t.c.store.ListMultipartUploads(ctx, bucket)
	return ups, t.c.classify(err)
}

// WalkMultipartUploads implements storage.UploadWalker.
func (b *Backend) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return err
	}
	if t.native() {
		return t.nat.WalkMultipartUploads(ctx, bucket, prefix, after, fn)
	}
	return t.c.walkErr(t.c.store.WalkMultipartUploads(ctx, bucket, prefix, after, markCallback(fn)))
}

func (b *Backend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if t.native() {
		return t.nat.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, b.withConfigs(bucket, opts))
	}
	info, err := t.c.store.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, opts)
	return info, t.c.classify(err)
}

func (b *Backend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return err
	}
	if t.native() {
		return t.nat.AbortMultipartUpload(ctx, bucket, key, uploadID)
	}
	return t.c.classify(t.c.store.AbortMultipartUpload(ctx, bucket, key, uploadID))
}

// --- write options, imports ---

// errNativeImport refuses imported versions on a native bucket.
func errNativeImport(bucket string) error {
	return fmt.Errorf("s3account: bucket %q is a native bucket (stored as-is in a remote bucket), which cannot receive copied versions: %w: %w",
		bucket, storage.ErrNotSupported, errors.ErrUnsupported)
}

// errNativeEncryption refuses encrypted writes to a native bucket.
func errNativeEncryption(bucket, mode string) error {
	return fmt.Errorf("s3account: bucket %q is a native bucket (stored as-is in a remote bucket), which cannot store %s-encrypted objects: %w",
		bucket, mode, errors.ErrUnsupported)
}

// checkNativeWrite refuses write options a native bucket cannot honour:
// encryption (never stored as plaintext instead) and invalid compression
// settings (valid ones are ignored: the data is stored as-is).
func checkNativeWrite(bucket string, o storage.WriteOptions) error {
	if o.Encryption != "" {
		return errNativeEncryption(bucket, o.Encryption)
	}
	switch c := o.Compression; c.Algorithm {
	case "":
	case storage.CompressionDeflate:
		if !compress.ValidLevel(c.Level) {
			return fmt.Errorf("s3account: invalid deflate level %d", c.Level)
		}
	default:
		return fmt.Errorf("s3account: unsupported compression algorithm %q", c.Algorithm)
	}
	return nil
}

// NewObjectWriterWith implements storage.CompressingBackend. Native
// buckets store data as-is: compression is not applied (the result reports
// it), encryption is refused (never stored as plaintext).
func (b *Backend) NewObjectWriterWith(ctx context.Context, bucket, key string, opts storage.WriteOptions) (storage.ObjectWriter, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if t.native() {
		if err := checkNativeWrite(bucket, opts); err != nil {
			return nil, err
		}
		return b.NewObjectWriter(ctx, bucket, key)
	}
	w, err := t.c.store.NewObjectWriterWith(ctx, bucket, key, opts)
	if err != nil {
		return nil, t.c.classify(err)
	}
	return t.c.wrapObjectWriter(w), nil
}

// CompleteMultipartUploadWith implements storage.CompressingBackend (see
// NewObjectWriterWith for native buckets).
func (b *Backend) CompleteMultipartUploadWith(ctx context.Context, bucket, key, uploadID string, partNumbers []int,
	meta storage.ObjectMeta, opts storage.CommitOptions, wopts storage.WriteOptions) (storage.ObjectInfo, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if t.native() {
		if err := checkNativeWrite(bucket, wopts); err != nil {
			return storage.ObjectInfo{}, err
		}
		return t.nat.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, b.withConfigs(bucket, opts))
	}
	info, err := t.c.store.CompleteMultipartUploadWith(ctx, bucket, key, uploadID, partNumbers, meta, opts, wopts)
	return info, t.c.classify(err)
}

// NewVersionWriter implements storage.VersionImporter (wrapped buckets
// only).
func (b *Backend) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if t.native() {
		return nil, errNativeImport(bucket)
	}
	w, err := t.c.store.NewVersionWriter(ctx, bucket, key)
	if err != nil {
		return nil, t.c.classify(err)
	}
	return t.c.wrapVersionWriter(w), nil
}

// NewVersionWriterWith implements storage.CompressingImporter (wrapped
// buckets only).
func (b *Backend) NewVersionWriterWith(ctx context.Context, bucket, key string, opts storage.WriteOptions) (storage.VersionWriter, error) {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if t.native() {
		return nil, errNativeImport(bucket)
	}
	w, err := t.c.store.NewVersionWriterWith(ctx, bucket, key, opts)
	if err != nil {
		return nil, t.c.classify(err)
	}
	return t.c.wrapVersionWriter(w), nil
}

// --- encryption ---

// PrepareEncryption implements storage.EncryptionBackend (wrapped buckets;
// native buckets answer errors.ErrUnsupported).
func (b *Backend) PrepareEncryption(ctx context.Context, bucket, mode string) error {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return err
	}
	if t.native() {
		return errNativeEncryption(bucket, mode)
	}
	return t.c.classify(t.c.store.PrepareEncryption(ctx, bucket, mode))
}

// ValidateEncryptionSettings implements storage.EncryptionBackend.
func (b *Backend) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	t, err := b.lookup(ctx, bucket)
	if err != nil {
		return err
	}
	if t.native() {
		return errNativeEncryption(bucket, cfg.Provider)
	}
	return t.c.classify(t.c.store.ValidateEncryptionSettings(ctx, bucket, cfg))
}

// EncryptionProviders implements storage.EncryptionProvidersBackend: none
// for a native bucket; a wrapped bucket's container's; for a bucket the
// backing does not hold (a replica or move target about to be created, in
// a container the caller does not name here), what its containers offer:
// they share the backing's connection and KEK, so any container that was
// opened answers for all (the default container preferred), whether or
// not it has loaded. A backing without containers offers none.
func (b *Backend) EncryptionProviders(ctx context.Context, bucket string) ([]string, error) {
	t, err := b.lookup(ctx, bucket)
	switch {
	case errors.Is(err, storage.ErrBucketNotFound):
		b.mu.RLock()
		c := b.containers[b.defaultContainer()]
		if c == nil || c.store == nil {
			c = nil
			for _, x := range b.containers {
				if x.store != nil && (c == nil || x.id < c.id) {
					c = x
				}
			}
		}
		b.mu.RUnlock()
		if c == nil {
			return []string{}, nil
		}
		return c.store.EncryptionProviders(ctx, bucket)
	case err != nil:
		return nil, err
	case t.native():
		return []string{}, nil
	}
	p, err := t.c.store.EncryptionProviders(ctx, bucket)
	return p, t.c.classify(err)
}
