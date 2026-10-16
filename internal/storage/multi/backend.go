package multi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- routing ---

// lookup returns the state of bucket. While the buckets of a backing are
// not known yet (see Router.pendingAdopt), a bucket without a placement
// may live there: it is reported unavailable (storage.ErrBackingUnavailable)
// rather than missing.
func (r *Router) lookup(bucket string) (*bucketState, error) {
	if r.closed.Load() {
		return nil, ErrClosed
	}
	r.mu.RLock()
	bs := r.buckets[bucket]
	var err error
	if bs == nil {
		err = r.unknownBucketLocked(bucket)
	}
	r.mu.RUnlock()
	if bs == nil {
		return nil, err
	}
	return bs, nil
}

// unknownBucketLocked is the error for bucket, which has no placement:
// storage.ErrBackingUnavailable while the buckets of a backing are not
// known yet, storage.ErrBucketNotFound otherwise. Router.mu must be held.
func (r *Router) unknownBucketLocked(bucket string) error {
	if len(r.pendingAdopt) == 0 {
		return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, bucket)
	}
	names := make([]string, 0, len(r.pendingAdopt))
	for name := range r.pendingAdopt {
		names = append(names, name)
	}
	slices.Sort(names)
	return fmt.Errorf("%w: bucket %q may be on backing %s, which is unavailable; the buckets on it are not known yet",
		storage.ErrBackingUnavailable, bucket, strings.Join(names, ", "))
}

// readers returns the backings a read of the bucket may use, best first:
// the primary, then (active-passive) synced replicas or (active-active)
// in-sync replicas, healthy backings before unhealthy ones. spread
// rotates the in-sync members of an active-active bucket, to spread
// object reads over them.
func (r *Router) readers(bs *bucketState, spread bool) []*backing {
	r.mu.RLock()
	rec := bs.rec
	cands := []*backing{bs.members[rec.primary()].backing}
	for _, m := range rec.Members[1:] {
		ms := bs.members[m.Backing]
		switch {
		case rec.Mode == ModeActiveActive && ms.syncMode:
		case rec.Mode == ModeActivePassive && m.Synced:
		default:
			continue
		}
		cands = append(cands, ms.backing)
	}
	r.mu.RUnlock()
	if spread && rec.Mode == ModeActiveActive && len(cands) > 1 {
		n := int(bs.rr.Add(1) % uint32(len(cands)))
		cands = append(cands[n:], cands[:n]...)
	}
	slices.SortStableFunc(cands, func(a, b *backing) int {
		ha, hb := a.healthy.Load(), b.healthy.Load()
		switch {
		case ha == hb:
			return 0
		case ha:
			return -1
		}
		return 1
	})
	return cands
}

// readFrom calls fn with the bucket's readers in order until one answers
// with something other than a backing failure or the unavailability of its
// copy's remote location (storage.ErrBucketUnavailable, which leaves the
// backing healthy). cb, when not nil, tells errors of the caller's
// callback apart.
func (r *Router) readFrom(bs *bucketState, spread bool, cb *cbTracker, fn func(b *backing) error) error {
	var first error
	for _, b := range r.readers(bs, spread) {
		err := fn(b)
		if !fallBack(err) || cb.isCaller(err) {
			return err
		}
		r.observe(b, err)
		if first == nil {
			first = err
		}
	}
	return first
}

// fallBack reports whether a read that failed with err should be retried
// on another copy of the bucket: after a failure of the backing, or of the
// copy's remote location alone (storage.ErrBucketUnavailable).
func fallBack(err error) bool {
	return isBackingError(err) || errors.Is(err, storage.ErrBucketUnavailable)
}

// route says where a write to a bucket goes.
type route struct {
	bs   *bucketState
	lead *backing
	// followers receive the write synchronously (in-sync active-active
	// replicas).
	followers []*memberState
	// async are the other members (and the move target): the changed key
	// is copied to them in the background.
	async []*memberState
}

// writeRoute computes the route of a write to bs.
func (r *Router) writeRoute(bs *bucketState) *route {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec := bs.rec
	rt := &route{bs: bs, lead: bs.members[rec.primary()].backing}
	for _, m := range rec.Members[1:] {
		ms := bs.members[m.Backing]
		if rec.Mode == ModeActiveActive && ms.syncMode {
			rt.followers = append(rt.followers, ms)
		} else {
			rt.async = append(rt.async, ms)
		}
	}
	if rec.Move != nil {
		rt.async = append(rt.async, bs.members[rec.Move.To])
	}
	return rt
}

// enterWrite looks the bucket up, passes its gate and computes the route.
// The caller must call leave.
func (r *Router) enterWrite(ctx context.Context, bucket string) (rt *route, leave func(), err error) {
	bs, err := r.lookup(bucket)
	if err != nil {
		return nil, nil, err
	}
	if err := bs.gate.enter(ctx); err != nil {
		return nil, nil, err
	}
	return r.writeRoute(bs), bs.gate.leave, nil
}

// inSyncMode reports whether ms receives synchronous writes.
func (r *Router) inSyncMode(ms *memberState) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return ms.syncMode
}

// demote takes an active-active replica out of synchronous writes after
// a failed write; key (when not "") is copied to it in the background.
func (r *Router) demote(ms *memberState, key string) {
	r.mu.Lock()
	ms.syncMode = false
	r.mu.Unlock()
	if key != "" {
		r.markDirty(ms, key)
	} else {
		r.markFull(ms)
	}
}

// replicate propagates a change of key, made on rt.lead, to the other
// members: synchronously to the followers (the caller holds the key's
// lock), in the background to the others (see settle).
func (r *Router) replicate(ctx context.Context, rt *route, key string) {
	committed := map[*memberState]bool{}
	for _, f := range rt.followers {
		err := r.syncKey(ctx, rt.lead, f.backing, rt.bs.name, key, false, nil)
		if err == nil {
			committed[f] = true
		}
		if errors.Is(err, errRestoredLocked) {
			r.restoredLocked(rt.bs, key)
		}
	}
	r.settle(rt, key, committed)
}

// objectWrite runs op, a change of bucket/key, on the bucket's primary and
// replicates it, then reports the change to the subscribers (see
// Subscribe). cb tells errors of the caller's callbacks apart from those
// of the backing.
func (r *Router) objectWrite(ctx context.Context, bucket, key string, cb *cbTracker, op func(b storage.Backend) error) error {
	if err := r.objectWriteGated(ctx, bucket, key, cb, op); err != nil {
		return err
	}
	r.emitObject(bucket, key)
	return nil
}

// objectWriteGated is objectWrite without reporting the change.
func (r *Router) objectWriteGated(ctx context.Context, bucket, key string, cb *cbTracker, op func(b storage.Backend) error) error {
	rt, leave, err := r.enterWrite(ctx, bucket)
	if err != nil {
		return err
	}
	defer leave()
	if len(rt.followers) > 0 {
		unlock := r.keys.lock(bucket, key)
		defer unlock()
	}
	if err := op(rt.lead.Backend); err != nil {
		if !cb.isCaller(err) {
			r.observe(rt.lead, err)
		}
		return err
	}
	r.replicate(ctx, rt, key)
	return nil
}

// bucketWrite applies op (a bucket-level change) to the primary, then to
// every other copy. A copy that fails gets a full comparison later. The
// change is reported to the subscribers as a ChangeBucketConfig of
// configKey (see Change.ConfigKey: "" for a bucket setting).
func (r *Router) bucketWrite(ctx context.Context, bucket, configKey string, op func(b storage.Backend) error) error {
	if err := r.bucketWriteGated(ctx, bucket, op); err != nil {
		return err
	}
	r.emit(Change{Kind: ChangeBucketConfig, Bucket: bucket, ConfigKey: configKey})
	return nil
}

// bucketWriteGated is bucketWrite without reporting the change.
func (r *Router) bucketWriteGated(ctx context.Context, bucket string, op func(b storage.Backend) error) error {
	rt, leave, err := r.enterWrite(ctx, bucket)
	if err != nil {
		return err
	}
	defer leave()
	if err := op(rt.lead.Backend); err != nil {
		return r.observe(rt.lead, err)
	}
	for _, ms := range append(slices.Clone(rt.followers), rt.async...) {
		if err := op(ms.backing.Backend); err != nil {
			r.observe(ms.backing, err)
			if r.inSyncMode(ms) {
				r.demote(ms, "")
			} else {
				r.markFull(ms)
			}
		}
	}
	return nil
}

// cbTracker remembers the last error a caller's callback returned, to
// tell it apart from backing failures.
type cbTracker struct {
	mu  sync.Mutex
	err error
}

func (t *cbTracker) note(err error) error {
	if err != nil && t != nil {
		t.mu.Lock()
		t.err = err
		t.mu.Unlock()
	}
	return err
}

func (t *cbTracker) isCaller(err error) bool {
	if t == nil || err == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err != nil && errors.Is(err, t.err)
}

// commitOptions returns opts with its BeforePublish routed through t, so
// a publication the caller refuses (authorization, retention) is not
// taken for a backing failure that would mark the backing unhealthy.
func (t *cbTracker) commitOptions(opts storage.CommitOptions) storage.CommitOptions {
	if bp := opts.BeforePublish; bp != nil {
		opts.BeforePublish = func(meta, current *storage.ObjectMeta, created time.Time, config func(string) []byte) error {
			return t.note(bp(meta, current, created, config))
		}
	}
	return opts
}

// --- buckets ---

// listWarnInterval rate-limits the warning about buckets ListBuckets
// leaves out.
const listWarnInterval = time.Minute

// ListBuckets returns every bucket, as its first reachable copy describes
// it. A bucket none of whose copies can be reached is described as it
// was last seen, or left out (with a rate-limited warning) when it was
// never seen: a failing backing does not fail the whole listing.
func (r *Router) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	if r.closed.Load() {
		return nil, ErrClosed
	}
	r.mu.RLock()
	states := make([]*bucketState, 0, len(r.buckets))
	for _, bs := range r.buckets {
		states = append(states, bs)
	}
	r.mu.RUnlock()
	sort.Slice(states, func(i, j int) bool { return states[i].name < states[j].name })

	lists := map[*backing]map[string]storage.BucketInfo{}
	errs := map[*backing]error{}
	list := func(b *backing) (map[string]storage.BucketInfo, error) {
		if m, ok := lists[b]; ok {
			return m, nil
		}
		if err, ok := errs[b]; ok {
			return nil, err
		}
		infos, err := b.Backend.ListBuckets(ctx)
		if err != nil {
			errs[b] = r.observe(b, err)
			return nil, err
		}
		m := make(map[string]storage.BucketInfo, len(infos))
		for _, in := range infos {
			m[in.Name] = in
		}
		lists[b] = m
		return m, nil
	}
	out := make([]storage.BucketInfo, 0, len(states))
	var skipped []string
	var skipErr error
	for _, bs := range states {
		var lastErr error
		found := false
		for _, b := range r.readers(bs, false) {
			m, err := list(b)
			if err != nil {
				lastErr = err
				continue
			}
			if in, ok := m[bs.name]; ok {
				bs.cacheInfo(in)
				in.Backing = r.primaryOf(bs)
				out = append(out, in)
				found = true
				break
			}
		}
		if found || lastErr == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if cached := bs.info.Load(); cached != nil {
			in := *cached
			in.Backing = r.primaryOf(bs)
			out = append(out, in)
			continue
		}
		skipped = append(skipped, bs.name)
		skipErr = lastErr
	}
	if len(skipped) > 0 {
		now := time.Now().UnixNano()
		if last := r.listWarnAt.Load(); now-last >= int64(listWarnInterval) && r.listWarnAt.CompareAndSwap(last, now) {
			r.log.Warn("buckets on unreachable backings are left out of the bucket list", "buckets", skipped, "err", skipErr)
		}
	}
	return out, nil
}

// CreateBucket creates the bucket on opts.Backing (the x-gos3-backing
// header), or on the default backing when it is empty. An unknown backing
// yields storage.ErrInvalidBacking. opts.Target (the bucket's kind and
// location) needs a backing with bucket kinds
// (storage.BucketCapabilities), else storage.ErrInvalidTarget; it is
// checked (storage.NewCapsOf) before anything is created. The default
// replicas are added except the chosen backing itself, when the new bucket
// keeps a version history to copy (not a native bucket) and they can
// receive copies. The new bucket is reported to the subscribers (see
// Subscribe).
func (r *Router) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	if err := r.createBucket(ctx, name, opts); err != nil {
		return err
	}
	r.emit(Change{Kind: ChangeBucketCreated, Bucket: name})
	return nil
}

// createBucket is CreateBucket without reporting the change.
func (r *Router) createBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	if r.closed.Load() {
		return ErrClosed
	}
	var (
		topology *backingTopology
		target   *backing
	)
	for {
		r.adminMu.Lock()
		topology = r.topology.Load()
		target = topology.def
		if opts.Backing != "" {
			if target = topology.backings[opts.Backing]; target == nil {
				r.adminMu.Unlock()
				return fmt.Errorf("%w: %q", storage.ErrInvalidBacking, opts.Backing)
			}
		}
		r.mu.RLock()
		exists := r.buckets[name] != nil
		pj := r.purges[purgeRecord{Bucket: name, Backing: target.Name}]
		var unknown error
		if !exists && len(r.pendingAdopt) > 0 {
			// The name may be taken on a backing not listed yet: creating
			// it elsewhere would shadow that bucket.
			unknown = r.unknownBucketLocked(name)
		}
		r.mu.RUnlock()
		if exists {
			r.adminMu.Unlock()
			return fmt.Errorf("%w: %q", storage.ErrBucketExists, name)
		}
		if unknown != nil {
			r.adminMu.Unlock()
			return unknown
		}
		if pj == nil {
			break
		}
		// A copy of a deleted bucket with this name is still being
		// removed from the target backing: wait for it.
		r.adminMu.Unlock()
		if err := r.waitPurge(ctx, pj.rec); err != nil {
			return err
		}
	}
	defer r.adminMu.Unlock()
	// The member backends do not know the router's backing names.
	opts.Backing = ""
	if opts.Target != nil {
		if _, ok := target.Backend.(storage.BucketCapabilities); !ok {
			return fmt.Errorf("%w: backing %q has no bucket kinds or locations to choose from", storage.ErrInvalidTarget, target.Name)
		}
	}
	// Refuse a target the backing cannot honour before creating anything.
	caps, err := storage.NewCapsOf(ctx, target.Backend, opts)
	if err != nil {
		return err
	}
	if err := target.Backend.CreateBucket(ctx, name, opts); err != nil {
		return r.observe(target, err)
	}
	rec := &placementRecord{
		Mode:    r.opts.NewBucketMode,
		Members: []memberRecord{{Backing: target.Name, Synced: true}},
	}
	for _, rep := range r.opts.NewBucketReplicas {
		if rep == target.Name {
			continue
		}
		rb := topology.backings[rep]
		var why error
		switch {
		case rb == nil:
			why = fmt.Errorf("%w %q", ErrUnknownBacking, rep)
		case !caps.History:
			why = errNoHistory(target, name, caps.Kind)
		default:
			why = importTarget(ctx, rb, nil)
		}
		if why != nil {
			// Open and live configuration refuse replicas that no new
			// bucket can have; this bucket's kind or backing rules them out.
			r.log.Warn("new bucket created without one of its default replicas, which cannot be used for it",
				"bucket", name, "backing", rep, "primary", target.Name, "reason", why)
			continue
		}
		r.mu.RLock()
		pending := r.purges[purgeRecord{Bucket: name, Backing: rep}] != nil
		r.mu.RUnlock()
		if pending || !rb.healthy.Load() {
			// Added later with SetReplication; mostly a stale copy.
			r.log.Warn("new bucket created without one of its default replicas", "bucket", name, "backing", rep,
				"copy_being_deleted", pending)
			continue
		}
		rec.Members = append(rec.Members, memberRecord{Backing: rep})
	}
	if len(rec.Members) == 1 {
		rec.Mode = ModeSingle
	}
	bs := r.newBucketState(name, rec)
	r.mu.Lock()
	r.buckets[name] = bs
	r.mu.Unlock()
	for _, ms := range bs.members {
		if ms.backing != target {
			r.kick(ms)
		}
	}
	// A failed save is repaired at the next start: buckets without a
	// placement are adopted where they are.
	r.saveLogged("creating a bucket")
	return nil
}

// purgePollInterval is how often waitPurge checks a purge.
const purgePollInterval = 200 * time.Millisecond

// waitPurge waits until the purge p finished.
func (r *Router) waitPurge(ctx context.Context, p purgeRecord) error {
	for {
		r.mu.RLock()
		pending := r.purges[p] != nil
		r.mu.RUnlock()
		if !pending {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.ctx.Done():
			return ErrClosed
		case <-time.After(purgePollInterval):
		}
	}
}

// GetBucket describes the bucket.
func (r *Router) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	bs, err := r.lookup(name)
	if err != nil {
		return storage.BucketInfo{}, err
	}
	var info storage.BucketInfo
	err = r.readFrom(bs, false, nil, func(b *backing) error {
		var err error
		info, err = b.Backend.GetBucket(ctx, name)
		return err
	})
	if err == nil {
		bs.cacheInfo(info)
		info.Backing = r.primaryOf(bs)
	}
	return info, err
}

// primaryOf returns the name of the backing holding bs's primary copy.
func (r *Router) primaryOf(bs *bucketState) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return bs.rec.primary()
}

// DeleteBucket deletes the (empty) bucket from its primary; the other
// copies are deleted in the background. The deletion is reported to the
// subscribers (see Subscribe).
func (r *Router) DeleteBucket(ctx context.Context, name string) error {
	if err := r.deleteBucket(ctx, name); err != nil {
		return err
	}
	r.emit(Change{Kind: ChangeBucketDeleted, Bucket: name})
	return nil
}

// deleteBucket is DeleteBucket without reporting the change.
func (r *Router) deleteBucket(ctx context.Context, name string) error {
	if r.closed.Load() {
		return ErrClosed
	}
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	bs, err := r.lookup(name)
	if err != nil {
		return err
	}
	r.mu.RLock()
	prim := bs.members[bs.rec.primary()].backing
	r.mu.RUnlock()
	if err := prim.Backend.DeleteBucket(ctx, name); err != nil {
		return r.observe(prim, err)
	}
	var cancel []*memberState
	r.mu.Lock()
	bs.removed = true
	delete(r.buckets, name)
	for bname, ms := range bs.members {
		cancel = append(cancel, ms)
		if bname != prim.Name {
			i := bs.rec.member(bname)
			r.addPurgeLocked(name, bname, i > 0 && bs.rec.Members[i].Diverged)
		}
	}
	r.mu.Unlock()
	for _, ms := range cancel {
		r.cancelMember(ms)
	}
	r.saveLogged("deleting a bucket")
	r.mu.RLock()
	for bname := range bs.members {
		r.startPurge(r.purges[purgeRecord{Bucket: name, Backing: bname}])
	}
	r.mu.RUnlock()
	return nil
}

// SetBucketVersioning changes the versioning state on every copy.
func (r *Router) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	return r.bucketWrite(ctx, name, "", func(b storage.Backend) error { return b.SetBucketVersioning(ctx, name, status) })
}

// SetBucketObjectLock enables object lock on every copy.
func (r *Router) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	return r.bucketWrite(ctx, name, "", func(b storage.Backend) error { return b.SetBucketObjectLock(ctx, name, enabled) })
}

// GetBucketConfig returns a configuration document.
func (r *Router) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	bs, err := r.lookup(bucket)
	if err != nil {
		return nil, err
	}
	var data []byte
	err = r.readFrom(bs, false, nil, func(b *backing) error {
		var err error
		data, err = b.Backend.GetBucketConfig(ctx, bucket, key)
		return err
	})
	return data, err
}

// ViewBucketConfig implements storage.ConfigViewer.
func (r *Router) ViewBucketConfig(ctx context.Context, bucket, key string, fn func(data []byte) error) error {
	bs, err := r.lookup(bucket)
	if err != nil {
		return err
	}
	cb := &cbTracker{}
	return r.readFrom(bs, false, cb, func(b *backing) error {
		var err error
		if cv, ok := b.Backend.(storage.ConfigViewer); ok {
			err = cv.ViewBucketConfig(ctx, bucket, key, func(data []byte) error { return cb.note(fn(data)) })
		} else {
			var data []byte
			if data, err = b.Backend.GetBucketConfig(ctx, bucket, key); err == nil {
				err = cb.note(fn(data))
			}
		}
		return err
	})
}

// PutBucketConfig stores a configuration document on every copy.
func (r *Router) PutBucketConfig(ctx context.Context, bucket, key string, data []byte) error {
	return r.bucketWrite(ctx, bucket, key, func(b storage.Backend) error { return b.PutBucketConfig(ctx, bucket, key, data) })
}

// DeleteBucketConfig removes a configuration document from every copy.
func (r *Router) DeleteBucketConfig(ctx context.Context, bucket, key string) error {
	return r.bucketWrite(ctx, bucket, key, func(b storage.Backend) error { return b.DeleteBucketConfig(ctx, bucket, key) })
}

// --- objects ---

// GetObjectInfo returns a version's metadata.
func (r *Router) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	bs, err := r.lookup(bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	var info storage.ObjectInfo
	err = r.readFrom(bs, true, nil, func(b *backing) error {
		var err error
		info, err = b.Backend.GetObjectInfo(ctx, bucket, key, versionID)
		return err
	})
	return info, err
}

// OpenObject opens a version for reading.
func (r *Router) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	bs, err := r.lookup(bucket)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	var (
		info storage.ObjectInfo
		rc   io.ReadSeekCloser
	)
	err = r.readFrom(bs, true, nil, func(b *backing) error {
		var err error
		info, rc, err = b.Backend.OpenObject(ctx, bucket, key, versionID)
		return err
	})
	return info, rc, err
}

// UpdateObjectMeta changes a version's metadata on every copy.
func (r *Router) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	var info storage.ObjectInfo
	cb := &cbTracker{}
	err := r.objectWrite(ctx, bucket, key, cb, func(b storage.Backend) error {
		var err error
		info, err = b.UpdateObjectMeta(ctx, bucket, key, versionID, func(m *storage.ObjectMeta) error { return cb.note(fn(m)) })
		return err
	})
	return info, err
}

// UpdateObjectMetaIf implements storage.ConditionalMetaUpdater.
func (r *Router) UpdateObjectMetaIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	var info storage.ObjectInfo
	cb := &cbTracker{}
	err := r.objectWrite(ctx, bucket, key, cb, func(b storage.Backend) error {
		cu, ok := b.(storage.ConditionalMetaUpdater)
		if !ok {
			return fmt.Errorf("conditional metadata updates: %w", errors.ErrUnsupported)
		}
		var err error
		info, err = cu.UpdateObjectMetaIf(ctx, bucket, key, versionID,
			func(oi storage.ObjectInfo) error { return cb.note(check(oi)) },
			func(m *storage.ObjectMeta) error { return cb.note(fn(m)) })
		return err
	})
	return info, err
}

// DeleteObject deletes per S3 semantics on the primary and replicates.
func (r *Router) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	var res storage.DeleteResult
	err := r.objectWrite(ctx, bucket, key, nil, func(b storage.Backend) error {
		var err error
		res, err = b.DeleteObject(ctx, bucket, key, versionID)
		return err
	})
	return res, err
}

// DeleteObjectIf implements storage.ConditionalDeleter.
func (r *Router) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	var res storage.DeleteResult
	cb := &cbTracker{}
	err := r.objectWrite(ctx, bucket, key, cb, func(b storage.Backend) error {
		cd, ok := b.(storage.ConditionalDeleter)
		if !ok {
			return fmt.Errorf("conditional deletes: %w", errors.ErrUnsupported)
		}
		var err error
		res, err = cd.DeleteObjectIf(ctx, bucket, key, versionID, func(oi storage.ObjectInfo) error { return cb.note(check(oi)) })
		return err
	})
	return res, err
}

// DeleteVersionIf implements storage.NewerVersionsDeleter.
func (r *Router) DeleteVersionIf(ctx context.Context, bucket, key, versionID string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, error) {
	var res storage.DeleteResult
	cb := &cbTracker{}
	err := r.objectWrite(ctx, bucket, key, cb, func(b storage.Backend) error {
		nd, ok := b.(storage.NewerVersionsDeleter)
		if !ok {
			return fmt.Errorf("newer-version conditional deletes: %w", errors.ErrUnsupported)
		}
		var err error
		res, err = nd.DeleteVersionIf(ctx, bucket, key, versionID, limit, func(v storage.ObjectInfo, newer []storage.ObjectInfo) error {
			return cb.note(check(v, newer))
		})
		return err
	})
	return res, err
}

// DeleteObjectChecked implements storage.CheckedDeleter.
func (r *Router) DeleteObjectChecked(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo, bool) error) (storage.DeleteResult, error) {
	var res storage.DeleteResult
	cb := &cbTracker{}
	err := r.objectWrite(ctx, bucket, key, cb, func(b storage.Backend) error {
		cd, ok := b.(storage.CheckedDeleter)
		if !ok {
			return fmt.Errorf("checked deletes: %w", errors.ErrUnsupported)
		}
		var err error
		res, err = cd.DeleteObjectChecked(ctx, bucket, key, versionID,
			func(oi storage.ObjectInfo, found bool) error { return cb.note(check(oi, found)) })
		return err
	})
	return res, err
}

// walk runs a walk on the bucket's readers. It falls back to the next
// reader (see fallBack) only when nothing was delivered yet.
func (r *Router) walk(bs *bucketState, fn func(storage.ObjectInfo) error, run func(b storage.Backend, fn func(storage.ObjectInfo) error) error) error {
	cands := r.readers(bs, false)
	cb := &cbTracker{}
	for i, b := range cands {
		delivered := false
		err := run(b.Backend, func(oi storage.ObjectInfo) error {
			delivered = true
			return cb.note(fn(oi))
		})
		if !fallBack(err) || cb.isCaller(err) {
			return err
		}
		r.observe(b, err)
		if delivered || i == len(cands)-1 {
			return err
		}
	}
	return nil
}

// WalkObjects walks the latest versions.
func (r *Router) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	bs, err := r.lookup(bucket)
	if err != nil {
		return err
	}
	return r.walk(bs, fn, func(b storage.Backend, fn func(storage.ObjectInfo) error) error {
		return b.WalkObjects(ctx, bucket, prefix, after, fn)
	})
}

// WalkVersions walks every version and delete marker.
func (r *Router) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	bs, err := r.lookup(bucket)
	if err != nil {
		return err
	}
	return r.walk(bs, fn, func(b storage.Backend, fn func(storage.ObjectInfo) error) error {
		return b.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn)
	})
}

// --- multipart uploads (primary only) ---

// primary returns the primary backing of bucket.
func (r *Router) primary(bucket string) (*backing, error) {
	bs, err := r.lookup(bucket)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return bs.members[bs.rec.primary()].backing, nil
}

// CreateMultipartUpload starts an upload on the primary.
func (r *Router) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	b, err := r.primary(bucket)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	u, err := b.Backend.CreateMultipartUpload(ctx, bucket, key, meta, initiator)
	return u, r.observe(b, err)
}

// GetMultipartUpload describes an upload.
func (r *Router) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	b, err := r.primary(bucket)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	u, err := b.Backend.GetMultipartUpload(ctx, bucket, key, uploadID)
	return u, r.observe(b, err)
}

// NewPartWriter starts writing a part.
func (r *Router) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	b, err := r.primary(bucket)
	if err != nil {
		return nil, err
	}
	w, err := b.Backend.NewPartWriter(ctx, bucket, key, uploadID, partNumber)
	return w, r.observe(b, err)
}

// ListParts lists an upload's parts.
func (r *Router) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	b, err := r.primary(bucket)
	if err != nil {
		return nil, err
	}
	ps, err := b.Backend.ListParts(ctx, bucket, key, uploadID)
	return ps, r.observe(b, err)
}

// ListMultipartUploads lists the bucket's uploads.
func (r *Router) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	b, err := r.primary(bucket)
	if err != nil {
		return nil, err
	}
	us, err := b.Backend.ListMultipartUploads(ctx, bucket)
	return us, r.observe(b, err)
}

// WalkMultipartUploads implements storage.UploadWalker.
func (r *Router) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	b, err := r.primary(bucket)
	if err != nil {
		return err
	}
	cb := &cbTracker{}
	err = storage.WalkMultipartUploads(ctx, b.Backend, bucket, prefix, after, func(u storage.UploadInfo) error { return cb.note(fn(u)) })
	if !cb.isCaller(err) {
		r.observe(b, err)
	}
	return err
}

// CompleteMultipartUpload completes an upload on the primary and
// replicates the new version.
func (r *Router) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	return r.CompleteMultipartUploadWith(ctx, bucket, key, uploadID, partNumbers, meta, opts, storage.WriteOptions{})
}

// CompleteMultipartUploadWith implements storage.CompressingBackend.
// Replicas store their copy compressed when the primary did.
func (r *Router) CompleteMultipartUploadWith(ctx context.Context, bucket, key, uploadID string, partNumbers []int,
	meta storage.ObjectMeta, opts storage.CommitOptions, wopts storage.WriteOptions) (storage.ObjectInfo, error) {
	if err := checkCompression(wopts.Compression); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := r.checkWriteEncryption(ctx, bucket, wopts); err != nil {
		return storage.ObjectInfo{}, err
	}
	var info storage.ObjectInfo
	cb := &cbTracker{}
	opts = cb.commitOptions(opts)
	err := r.objectWrite(ctx, bucket, key, cb, func(b storage.Backend) error {
		var err error
		if !withOptions(wopts) {
			info, err = b.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, opts)
			return err
		}
		cb, ok := b.(storage.CompressingBackend)
		if !ok {
			return errNoWriteOptions("", wopts)
		}
		info, err = cb.CompleteMultipartUploadWith(ctx, bucket, key, uploadID, partNumbers, meta, opts, wopts)
		return err
	})
	return info, err
}

// AbortMultipartUpload aborts an upload.
func (r *Router) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	b, err := r.primary(bucket)
	if err != nil {
		return err
	}
	return r.observe(b, b.Backend.AbortMultipartUpload(ctx, bucket, key, uploadID))
}
