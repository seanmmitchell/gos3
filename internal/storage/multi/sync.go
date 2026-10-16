package multi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
)

// errSourceChanged: a key changed on the source while it was being
// copied; it is copied again.
var errSourceChanged = errors.New("multi: source changed during copy")

// errRestoredLocked: syncKey copied locked versions that only the
// destination had back to the source (the primary). The key is copied
// again, and every other copy of the bucket must receive them too (see
// restoredLocked).
var errRestoredLocked = fmt.Errorf("multi: locked versions restored to the primary: %w", errSourceChanged)

// errLockedConflict: the destination holds a locked version under an ID
// for which the source holds a different version (a null version written
// on both sides of a failover). Neither may be deleted or replaced, so the
// key is left as it is on the destination.
var errLockedConflict = errors.New("multi: a locked version conflicts with the primary's")

// walkBatchKeys is the number of keys a full comparison collects per walk.
const walkBatchKeys = 256

// syncMemberKey copies key from the bucket's primary to member ms, under
// the key's lock. prog, when non-nil, counts the work.
func (r *Router) syncMemberKey(ctx context.Context, ms *memberState, key string, prog *Progress) error {
	r.mu.RLock()
	rec := ms.bucket.rec
	src := ms.bucket.members[rec.primary()].backing
	removed := ms.bucket.removed
	i := rec.member(ms.backing.Name)
	diverged := i > 0 && rec.Members[i].Diverged
	r.mu.RUnlock()
	if removed || ms.isCanceled() {
		return errCanceled
	}
	if src == ms.backing {
		return nil // promoted meanwhile
	}
	unlock := r.keys.lock(ms.bucket.name, key)
	defer unlock()
	err := r.syncKey(ctx, src, ms.backing, ms.bucket.name, key, diverged, prog)
	if errors.Is(err, errRestoredLocked) {
		r.restoredLocked(ms.bucket, key)
	}
	if errors.Is(err, errSourceChanged) {
		r.markDirty(ms, key)
		return nil
	}
	if errors.Is(err, errLockedConflict) {
		r.log.Error("a locked version on a bucket copy conflicts with the primary's; leaving the key as it is there",
			"bucket", ms.bucket.name, "key", key, "backing", ms.backing.Name, "err", err)
		return nil
	}
	return err
}

// restoredLocked reacts to syncKey having copied locked versions of key
// back to the primary of bs: every other copy is brought up to date in the
// background, and subscribers learn of the change.
func (r *Router) restoredLocked(bs *bucketState, key string) {
	r.mu.RLock()
	primary := bs.rec.primary()
	others := make([]*memberState, 0, len(bs.members))
	for name, ms := range bs.members {
		if name != primary {
			others = append(others, ms)
		}
	}
	r.mu.RUnlock()
	for _, ms := range others {
		r.markDirty(ms, key)
	}
	r.emitObject(bs.name, key)
}

// syncKey makes the versions of bucket/key on dst identical to those on
// src (IDs, order, data, metadata). The caller holds the key's lock.
//
// Versions are compared by identity (ID, LastModified, ETag, size,
// delete marker). dst versions that do not exist on src are deleted;
// then, oldest first, the longest common prefix of both histories is
// kept (with metadata updated where it differs) and the rest of dst is
// replaced by the rest of src. In the common case (new versions added on
// src) that copies just the new versions.
//
// With restore (dst is a Diverged member: the old primary after a forced
// promote or a failover to a lagging replica), a dst version that object
// lock protects (legal hold, unexpired retention) and src does not have is
// never deleted: it can be an acknowledged write the new primary never
// received, and the S3 layer, which enforces the lock for clients, is not
// involved here. It is copied back to src instead (errRestoredLocked). If
// src holds a different version under its ID, dst is left unchanged
// (errLockedConflict). Without restore, src is authoritative: a locked
// version it lacks was deleted there legitimately (by a client allowed to
// bypass governance retention, say), and is deleted on dst too.
func (r *Router) syncKey(ctx context.Context, src, dst *backing, bucket, key string, restore bool, prog *Progress) error {
	sv, err := copier.KeyVersions(ctx, src.Backend, bucket, key)
	if err != nil {
		return r.observe(src, fmt.Errorf("listing %s/%s on backing %q: %w", bucket, key, src.Name, err))
	}
	dv, err := copier.KeyVersions(ctx, dst.Backend, bucket, key)
	if err != nil {
		return r.observe(dst, fmt.Errorf("listing %s/%s on backing %q: %w", bucket, key, dst.Name, err))
	}
	if prog != nil {
		prog.Keys++
	}
	srcByID := make(map[string]storage.ObjectInfo, len(sv))
	for _, v := range sv {
		srcByID[v.VersionID] = v
	}
	if restore {
		if err := r.restoreLocked(ctx, src, dst, bucket, key, srcByID, dv); err != nil {
			return err
		}
	}
	del := func(id string) error {
		_, err := dst.Backend.DeleteObject(ctx, bucket, key, id)
		if err != nil && !errors.Is(err, storage.ErrVersionNotFound) && !errors.Is(err, storage.ErrObjectNotFound) {
			return r.observe(dst, fmt.Errorf("deleting %s/%s version %s on backing %q: %w", bucket, key, id, dst.Name, err))
		}
		return nil
	}
	// Oldest first from here on.
	slices.Reverse(sv)
	slices.Reverse(dv)
	keep := dv[:0:0]
	for _, d := range dv {
		if s, ok := srcByID[d.VersionID]; ok && copier.SameVersion(s, d) {
			keep = append(keep, d)
			continue
		}
		if err := del(d.VersionID); err != nil {
			return err
		}
	}
	common := 0
	for common < len(keep) && common < len(sv) && keep[common].VersionID == sv[common].VersionID {
		common++
	}
	for _, d := range keep[common:] {
		if err := del(d.VersionID); err != nil {
			return err
		}
	}
	for i := range common {
		s, d := sv[i], keep[i]
		if s.DeleteMarker || copier.MetaEqual(s.ObjectMeta, d.ObjectMeta) {
			continue
		}
		meta := copier.CloneMeta(s.ObjectMeta)
		_, err := dst.Backend.UpdateObjectMeta(ctx, bucket, key, s.VersionID, func(m *storage.ObjectMeta) error {
			*m = copier.CloneMeta(meta)
			return nil
		})
		if err != nil && !errors.Is(err, storage.ErrVersionNotFound) {
			return r.observe(dst, fmt.Errorf("updating %s/%s version %s on backing %q: %w", bucket, key, s.VersionID, dst.Name, err))
		}
	}
	changed := false
	for _, s := range sv[common:] {
		n, err := r.importVersion(ctx, src, dst, bucket, key, s)
		if errors.Is(err, errSourceChanged) {
			changed = true
			continue
		}
		if err != nil {
			return err
		}
		if prog != nil {
			prog.Versions++
			prog.Bytes += n
		}
	}
	if changed {
		return errSourceChanged
	}
	return nil
}

// restoreLocked copies the locked versions in dv (dst's versions of
// bucket/key) that src does not have back to src, oldest first, and then
// returns errRestoredLocked; it returns errLockedConflict when src has a
// different version under a locked version's ID, and nil when no locked
// version is missing from src.
func (r *Router) restoreLocked(ctx context.Context, src, dst *backing, bucket, key string, srcByID map[string]storage.ObjectInfo, dv []storage.ObjectInfo) error {
	now := time.Now()
	var restore []storage.ObjectInfo
	for _, d := range dv {
		s, ok := srcByID[d.VersionID]
		if ok && copier.SameVersion(s, d) {
			continue
		}
		why := copier.LockReason(d, now)
		if why == "" {
			continue
		}
		if ok {
			return fmt.Errorf("%w: %s/%s version %s on backing %q is %s, and backing %q has another version under that ID",
				errLockedConflict, bucket, key, d.VersionID, dst.Name, why, src.Name)
		}
		restore = append(restore, d)
	}
	if len(restore) == 0 {
		return nil
	}
	for _, d := range slices.Backward(restore) { // dv is newest first
		r.log.Warn("restoring a locked version that only a bucket copy has to the primary",
			"bucket", bucket, "key", key, "version", d.VersionID, "from", dst.Name, "to", src.Name)
		if _, err := r.importVersion(ctx, dst, src, bucket, key, d); err != nil {
			return err
		}
	}
	return errRestoredLocked
}

// importVersion copies version v of bucket/key from src to dst. It returns
// the number of bytes copied.
func (r *Router) importVersion(ctx context.Context, src, dst *backing, bucket, key string, v storage.ObjectInfo) (int64, error) {
	opts := storage.WriteOptions{Encryption: v.Encryption}
	if v.Compression == storage.CompressionDeflate {
		// Keep the copy compressed like its source (the level the source
		// used is not recorded).
		opts.Compression = storage.Compression{Algorithm: storage.CompressionDeflate, Level: compress.DefaultLevel}
	}
	w, err := newVersionWriter(ctx, dst, bucket, key, opts)
	if err != nil {
		return 0, r.observe(dst, fmt.Errorf("copying %s/%s to backing %q: %w", bucket, key, dst.Name, err))
	}
	info := v
	var n int64
	if !v.DeleteMarker {
		oi, rc, err := src.Backend.OpenObject(ctx, bucket, key, v.VersionID)
		if err != nil {
			_ = w.Abort()
			if errors.Is(err, storage.ErrVersionNotFound) || errors.Is(err, storage.ErrObjectNotFound) {
				return 0, errSourceChanged
			}
			return 0, r.observe(src, fmt.Errorf("reading %s/%s version %s on backing %q: %w", bucket, key, v.VersionID, src.Name, err))
		}
		if oi.Encryption != opts.Encryption {
			if rc != nil {
				_ = rc.Close()
			}
			_ = w.Abort()
			return 0, errSourceChanged
		}
		if rc != nil { // nil: the null version became a delete marker meanwhile
			n, err = io.Copy(w, rc)
			_ = rc.Close()
		}
		if err != nil {
			_ = w.Abort()
			return 0, fmt.Errorf("copying %s/%s version %s from %q to %q: %w", bucket, key, v.VersionID, src.Name, dst.Name, err)
		}
		// Store what was read: data and metadata are consistent even if
		// the null version was replaced in between (then the key is
		// copied again).
		info = oi
	}
	if _, err := w.Commit(ctx, info); err != nil {
		return 0, r.observe(dst, fmt.Errorf("copying %s/%s version %s to backing %q: %w", bucket, key, v.VersionID, dst.Name, err))
	}
	if !copier.SameVersion(info, v) {
		return n, errSourceChanged
	}
	return n, nil
}

// newVersionWriter starts an imported version on b, stored as opts asks
// when b can compress (else as is). errors.ErrUnsupported when b cannot
// import versions at all.
func newVersionWriter(ctx context.Context, b *backing, bucket, key string, opts storage.WriteOptions) (storage.VersionWriter, error) {
	if withOptions(opts) {
		if ci, ok := b.Backend.(storage.CompressingImporter); ok {
			return ci.NewVersionWriterWith(ctx, bucket, key, opts)
		}
		if opts.Encryption != "" {
			return nil, errNoWriteOptions(b.Name, opts)
		}
	}
	imp, ok := b.Backend.(storage.VersionImporter)
	if !ok {
		return nil, fmt.Errorf("backing %q cannot receive copies: %w", b.Name, errors.ErrUnsupported)
	}
	return imp.NewVersionWriter(ctx, bucket, key)
}

// walkKeys calls fn with the keys of bucket on b (each key once,
// ascending) in batches, outside of the walk, so fn may call into any
// backing.
func walkKeys(ctx context.Context, b storage.Backend, bucket string, fn func(keys []string) error) error {
	after := ""
	for {
		var keys []string
		more := false
		err := b.WalkVersions(ctx, bucket, "", after, "", func(oi storage.ObjectInfo) error {
			if n := len(keys); n > 0 && keys[n-1] == oi.Key {
				return nil
			}
			if len(keys) == walkBatchKeys {
				more = true
				return storage.ErrStopWalk
			}
			keys = append(keys, oi.Key)
			return nil
		})
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := fn(keys); err != nil {
				// fn reports (and observes) its own backings' errors.
				return &callerError{err: err}
			}
		}
		if !more {
			return nil
		}
		after = keys[len(keys)-1]
	}
}

// fullSync compares member ms with the bucket's primary in full: bucket
// settings and configuration, every key present on either side, and
// in-progress uploads (removed from ms: uploads live on the primary only).
func (r *Router) fullSync(ctx context.Context, ms *memberState) error {
	bs := ms.bucket
	r.mu.RLock()
	src := bs.members[bs.rec.primary()].backing
	// The target of a move is created where the move asked; replicas
	// where their backing puts copies.
	var target *storage.BucketTarget
	if mv := bs.rec.Move; mv != nil && mv.To == ms.backing.Name {
		target = cloneTarget(mv.Target) // records are immutable
	}
	r.mu.RUnlock()
	if src == ms.backing {
		return nil
	}
	if !src.healthy.Load() {
		return fmt.Errorf("primary backing %q is unhealthy", src.Name)
	}
	dst := ms.backing
	if err := r.syncBucket(ctx, src, dst, bs.name, target); err != nil {
		return err
	}
	if err := abortUploads(ctx, dst, bs.name, r); err != nil {
		return err
	}
	canceled := func() error {
		if ms.isCanceled() {
			return errCanceled
		}
		return ctx.Err()
	}
	prog := func() *Progress {
		ms.mu.Lock()
		defer ms.mu.Unlock()
		return ms.progress
	}
	// Every key of the source.
	err := walkKeys(ctx, src.Backend, bs.name, func(keys []string) error {
		for _, k := range keys {
			if err := canceled(); err != nil {
				return err
			}
			p := &Progress{}
			if err := r.syncMemberKey(ctx, ms, k, p); err != nil {
				return err
			}
			r.addProgress(ms, prog(), p)
		}
		return nil
	})
	if err != nil {
		return r.walkErr(src, err)
	}
	// Keys only the member has.
	err = walkKeys(ctx, dst.Backend, bs.name, func(keys []string) error {
		for _, k := range keys {
			if err := canceled(); err != nil {
				return err
			}
			_, err := src.Backend.GetObjectInfo(ctx, bs.name, k, "")
			if err == nil {
				continue
			}
			if !errors.Is(err, storage.ErrObjectNotFound) {
				return r.observe(src, err)
			}
			if err := r.syncMemberKey(ctx, ms, k, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return r.walkErr(dst, err)
	}
	return nil
}

func (r *Router) addProgress(ms *memberState, total, p *Progress) {
	if total == nil {
		return
	}
	ms.mu.Lock()
	total.Keys += p.Keys
	total.Versions += p.Versions
	total.Bytes += p.Bytes
	ms.mu.Unlock()
}

// walkErr observes a walk error unless it came from the walk callback
// (which reports its own backing's errors).
func (r *Router) walkErr(b *backing, err error) error {
	if errors.Is(err, errCanceled) || errors.Is(err, context.Canceled) {
		return err
	}
	return r.observe(b, err)
}

// syncBucket makes the bucket on dst match src: existence, versioning,
// object lock and every configuration document. A missing bucket is
// created on dst to receive imported versions, with target (the kind and
// location a move asked for; nil: the backing's choice for copies).
func (r *Router) syncBucket(ctx context.Context, src, dst *backing, bucket string, target *storage.BucketTarget) error {
	info, err := src.Backend.GetBucket(ctx, bucket)
	if err != nil {
		return r.observe(src, err)
	}
	cur, err := dst.Backend.GetBucket(ctx, bucket)
	if errors.Is(err, storage.ErrBucketNotFound) {
		err = dst.Backend.CreateBucket(ctx, bucket, storage.CreateBucketOptions{
			Region: info.Region, ObjectLockEnabled: info.ObjectLockEnabled, Owner: info.Owner,
			Import: true, Target: target,
		})
		if err != nil && !errors.Is(err, storage.ErrBucketExists) {
			return r.observe(dst, fmt.Errorf("creating bucket %q on backing %q: %w", bucket, dst.Name, err))
		}
		cur, err = dst.Backend.GetBucket(ctx, bucket)
	}
	if err != nil {
		return r.observe(dst, err)
	}
	if cur.Versioning != info.Versioning && info.Versioning != storage.VersioningUnversioned {
		if err := dst.Backend.SetBucketVersioning(ctx, bucket, info.Versioning); err != nil {
			return r.observe(dst, err)
		}
	}
	if cur.ObjectLockEnabled != info.ObjectLockEnabled {
		if err := dst.Backend.SetBucketObjectLock(ctx, bucket, info.ObjectLockEnabled); err != nil {
			return r.observe(dst, err)
		}
	}
	for _, k := range storage.AllConfigKeys {
		if err := r.syncConfig(ctx, src, dst, bucket, k); err != nil {
			return err
		}
	}
	return nil
}

// syncConfig copies one configuration document from src to dst.
func (r *Router) syncConfig(ctx context.Context, src, dst *backing, bucket, key string) error {
	want, err := src.Backend.GetBucketConfig(ctx, bucket, key)
	if err != nil && !errors.Is(err, storage.ErrConfigNotFound) {
		return r.observe(src, err)
	}
	missing := errors.Is(err, storage.ErrConfigNotFound)
	have, err := dst.Backend.GetBucketConfig(ctx, bucket, key)
	if err != nil && !errors.Is(err, storage.ErrConfigNotFound) {
		return r.observe(dst, err)
	}
	dstMissing := errors.Is(err, storage.ErrConfigNotFound)
	switch {
	case missing && dstMissing:
		return nil
	case missing:
		return r.observe(dst, dst.Backend.DeleteBucketConfig(ctx, bucket, key))
	case !dstMissing && bytes.Equal(want, have):
		return nil
	}
	return r.observe(dst, dst.Backend.PutBucketConfig(ctx, bucket, key, want))
}

// abortUploads aborts every in-progress multipart upload of bucket on b.
func abortUploads(ctx context.Context, b *backing, bucket string, r *Router) error {
	ups, err := b.Backend.ListMultipartUploads(ctx, bucket)
	if err != nil {
		return r.observe(b, err)
	}
	for _, u := range ups {
		err := b.Backend.AbortMultipartUpload(ctx, bucket, u.Key, u.UploadID)
		if err != nil && !errors.Is(err, storage.ErrUploadNotFound) {
			return r.observe(b, err)
		}
	}
	return nil
}

// --- purges ---

// purgeJob deletes a bucket's copy from a backing in the background.
type purgeJob struct {
	rec     purgeRecord
	running bool // guarded by Router.mu
	// diverged: the copy was a Diverged member (guarded by Router.mu; see
	// purgeLocked).
	diverged bool
}

// addPurgeLocked records a copy to delete; diverged says it was a Diverged
// member. Router.mu must be held (or Open must be running).
func (r *Router) addPurgeLocked(bucket, backingName string, diverged bool) {
	p := purgeRecord{Bucket: bucket, Backing: backingName}
	if r.purges[p] == nil {
		r.purges[p] = &purgeJob{rec: p}
	}
	r.purges[p].diverged = r.purges[p].diverged || diverged
}

// startPurge runs j in the background unless it runs already. Router.mu
// must be held (read or write).
func (r *Router) startPurge(j *purgeJob) {
	if j == nil || j.running || r.ctx.Err() != nil {
		return
	}
	j.running = true
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		backoff := minBackoff
		for {
			r.mu.RLock()
			diverged := j.diverged
			r.mu.RUnlock()
			err := r.purge(r.ctx, j.rec, diverged)
			if err == nil {
				r.mu.Lock()
				delete(r.purges, j.rec)
				r.mu.Unlock()
				r.log.Info("deleted a bucket copy", "bucket", j.rec.Bucket, "backing", j.rec.Backing)
				r.saveLogged("deleting a bucket copy")
				return
			}
			if r.ctx.Err() != nil {
				return
			}
			r.log.Warn("deleting a bucket copy failed; retrying", "bucket", j.rec.Bucket,
				"backing", j.rec.Backing, "retry_in", backoff, "err", err)
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
		}
	}()
}

// purgeLocked lets purge delete version v of bucket's copy on backing b.
// A copy that was not a Diverged member only holds what the primary
// holds or held (a locked version the primary lacks was deleted there
// legitimately), so everything goes. Of a diverged copy, a version under
// object lock goes only if the bucket's primary holds the same version; a
// locked version the primary does not have is copied to it first, and
// one that cannot be kept elsewhere (the bucket is gone, or its primary
// holds another version under that ID) fails the purge, which is retried,
// so the copy stays until the lock ends.
func (r *Router) purgeLocked(ctx context.Context, b *backing, bucket string, v storage.ObjectInfo, diverged bool) error {
	why := copier.LockReason(v, time.Now())
	if !diverged || why == "" {
		return nil
	}
	r.mu.RLock()
	bs := r.buckets[bucket]
	var prim *backing
	if bs != nil && !bs.removed {
		if ms := bs.members[bs.rec.primary()]; ms != nil {
			prim = ms.backing
		}
	}
	r.mu.RUnlock()
	if prim == nil || prim == b {
		return fmt.Errorf("not deleting %s/%s version %s on backing %q: it is %s", bucket, v.Key, v.VersionID, b.Name, why)
	}
	unlock := r.keys.lock(bucket, v.Key)
	defer unlock()
	pv, err := prim.Backend.GetObjectInfo(ctx, bucket, v.Key, v.VersionID)
	switch {
	case err == nil && copier.SameVersion(pv, v):
		return nil
	case err == nil:
		return fmt.Errorf("not deleting %s/%s version %s on backing %q: it is %s, and the primary %q has another version under that ID",
			bucket, v.Key, v.VersionID, b.Name, why, prim.Name)
	case !errors.Is(err, storage.ErrVersionNotFound) && !errors.Is(err, storage.ErrObjectNotFound):
		return r.observe(prim, err)
	}
	r.log.Warn("restoring a locked version that only a deleted bucket copy has to the primary",
		"bucket", bucket, "key", v.Key, "version", v.VersionID, "from", b.Name, "to", prim.Name)
	if _, err := r.importVersion(ctx, b, prim, bucket, v.Key, v); err != nil {
		return err
	}
	r.restoredLocked(bs, v.Key)
	return nil
}

// purge deletes every version, upload and configuration document of the
// bucket on the backing, then the bucket itself; diverged says the copy
// was a Diverged member (see purgeLocked).
func (r *Router) purge(ctx context.Context, p purgeRecord, diverged bool) error {
	b := r.topology.Load().backings[p.Backing]
	if b == nil {
		return nil
	}
	_, err := b.Backend.GetBucket(ctx, p.Bucket)
	if errors.Is(err, storage.ErrBucketNotFound) {
		return nil
	}
	if err != nil {
		return r.observe(b, err)
	}
	if err := abortUploads(ctx, b, p.Bucket, r); err != nil {
		return err
	}
	for {
		var vs []storage.ObjectInfo
		err := b.Backend.WalkVersions(ctx, p.Bucket, "", "", "", func(oi storage.ObjectInfo) error {
			vs = append(vs, oi)
			if len(vs) == 1000 {
				return storage.ErrStopWalk
			}
			return nil
		})
		if err != nil {
			return r.observe(b, err)
		}
		if len(vs) == 0 {
			break
		}
		for _, v := range vs {
			if err := r.purgeLocked(ctx, b, p.Bucket, v, diverged); err != nil {
				return err
			}
			_, err := b.Backend.DeleteObject(ctx, p.Bucket, v.Key, v.VersionID)
			if err != nil && !errors.Is(err, storage.ErrVersionNotFound) && !errors.Is(err, storage.ErrObjectNotFound) {
				return r.observe(b, err)
			}
		}
	}
	err = b.Backend.DeleteBucket(ctx, p.Bucket)
	if err != nil && !errors.Is(err, storage.ErrBucketNotFound) {
		return r.observe(b, err)
	}
	return nil
}
