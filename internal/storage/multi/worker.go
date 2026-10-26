package multi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage/copier"
)

// Move phases.
const (
	phaseCopying = "copying"
	phaseCutover = "cutover"
)

// maxDirtyKeys bounds the changed keys remembered per member; beyond it
// the member falls back to a full comparison.
const maxDirtyKeys = 100_000

// syncBatch is the number of changed keys a worker copies per turn.
const syncBatch = 64

// Retry back-off of a failing member.
const (
	minBackoff = time.Second
	maxBackoff = time.Minute
)

// errCanceled stops the work of a member that was removed.
var errCanceled = errors.New("multi: copy canceled")

// memberState is the runtime state of one backing's copy of a bucket (a
// member or the move target).
type memberState struct {
	bucket  *bucketState
	backing *backing

	// syncMode (active-active replicas only; guarded by Router.mu): the
	// member receives every write synchronously and serves reads.
	syncMode bool

	mu         sync.Mutex
	dirty      map[string]struct{} // changed keys to copy
	needFull   bool                // a full comparison is due
	running    bool                // queued or being worked on by a worker
	canceled   bool                // removed: stop working
	fullDone   bool                // a full comparison completed since the member was (re)started
	failures   int                 // consecutive failures
	retry      *time.Timer         // pending retry after a failure
	progress   *Progress           // running full comparison
	lastSyncAt time.Time           // last completed full comparison
	lastErr    string
	// skipped holds the keys that could not be copied because a copy
	// holds more than copier.MaxKeyVersions versions of them, with the
	// error. The member's other keys are copied, but while any key is
	// skipped the member is degraded and onIdle completes nothing.
	skipped   map[string]error
	nskipped  atomic.Int32 // len(skipped), for readers that take no lock (see skippedKeys)
	skipRetry *time.Timer  // pending retry of the skipped keys
}

// skippedRetryInterval is how long a member with skipped keys waits before
// it tries them again (they are also tried whenever they change, and by
// every full comparison). A variable for tests.
var skippedRetryInterval = 5 * time.Minute

// Progress counts the work of a running full copy.
type Progress struct {
	Keys     int64 `json:"keys"`     // keys compared
	Versions int64 `json:"versions"` // versions copied
	Bytes    int64 `json:"bytes"`    // bytes copied
}

func (r *Router) newMemberState(bs *bucketState, backingName string) *memberState {
	return &memberState{bucket: bs, backing: r.topology.Load().backings[backingName], dirty: map[string]struct{}{},
		skipped: map[string]error{}}
}

// skipKey records that key could not be copied to ms because its history
// is too long (err wraps copier.ErrTooManyVersions), so that the copy of
// the other keys can go on. This says nothing about the backings' health.
func (r *Router) skipKey(ms *memberState, key string, err error) {
	ms.mu.Lock()
	_, known := ms.skipped[key]
	ms.skipped[key] = err
	ms.nskipped.Store(int32(len(ms.skipped)))
	ms.mu.Unlock()
	if !known {
		r.log.Warn("a key has too many versions to be copied to a bucket copy; skipping it until it changes",
			"bucket", ms.bucket.name, "backing", ms.backing.Name, "key", key, "err", err)
	}
}

// unskip forgets that key was skipped (it was copied).
func (ms *memberState) unskip(key string) {
	ms.mu.Lock()
	delete(ms.skipped, key)
	ms.nskipped.Store(int32(len(ms.skipped)))
	ms.mu.Unlock()
}

// skippedKeys returns the number of skipped keys without taking ms.mu, so
// that the read path (readers) does not contend with the worker.
func (ms *memberState) skippedKeys() int { return int(ms.nskipped.Load()) }

// retrySkipped marks the member's skipped keys changed after
// skippedRetryInterval, unless a retry is pending already. A key's history
// can shrink without the router seeing it (a native bucket changed on its
// own side, say).
func (r *Router) retrySkipped(ms *memberState) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.skipRetry != nil || ms.canceled || len(ms.skipped) == 0 {
		return
	}
	ms.skipRetry = time.AfterFunc(skippedRetryInterval, func() {
		ms.mu.Lock()
		ms.skipRetry = nil
		keys := make([]string, 0, len(ms.skipped))
		for k := range ms.skipped {
			keys = append(keys, k)
		}
		ms.mu.Unlock()
		for _, k := range keys {
			r.markDirty(ms, k)
		}
	})
}

// markDirty records that key changed and must be copied to the member.
func (r *Router) markDirty(ms *memberState, key string) {
	ms.mu.Lock()
	if !ms.needFull {
		if len(ms.dirty) >= maxDirtyKeys {
			ms.needFull = true
			ms.dirty = map[string]struct{}{}
		} else {
			ms.dirty[key] = struct{}{}
		}
	}
	ms.mu.Unlock()
	r.kick(ms)
}

// markFull schedules a full comparison of the member.
func (r *Router) markFull(ms *memberState) {
	ms.mu.Lock()
	ms.needFull = true
	ms.mu.Unlock()
	r.kick(ms)
}

// kick queues the member for a worker unless it is queued or running
// already, or waits for a retry.
func (r *Router) kick(ms *memberState) {
	ms.mu.Lock()
	if ms.running || ms.canceled || ms.retry != nil || r.jobs == nil {
		ms.mu.Unlock()
		return
	}
	ms.running = true
	ms.mu.Unlock()
	r.jobs.push(ms)
}

// cancelMember stops the member's work (it was removed).
func (r *Router) cancelMember(ms *memberState) {
	ms.mu.Lock()
	ms.canceled = true
	if ms.retry != nil {
		ms.retry.Stop()
		ms.retry = nil
	}
	if ms.skipRetry != nil {
		ms.skipRetry.Stop()
		ms.skipRetry = nil
	}
	ms.dirty = map[string]struct{}{}
	ms.mu.Unlock()
}

// workQueue is an unbounded FIFO of members to work on.
type workQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []*memberState
	closed bool
}

func newWorkQueue() *workQueue {
	q := &workQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *workQueue) push(ms *memberState) {
	q.mu.Lock()
	if !q.closed {
		q.items = append(q.items, ms)
		q.cond.Signal()
	}
	q.mu.Unlock()
}

// pop waits for an item; nil once the queue is closed.
func (q *workQueue) pop() *memberState {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return nil
	}
	ms := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	return ms
}

func (q *workQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (r *Router) runWorker() {
	for {
		ms := r.jobs.pop()
		if ms == nil {
			return
		}
		r.work(ms)
	}
}

// work copies the member's pending changes until there are none, then
// completes whatever the member was being prepared for (see onIdle). It
// runs with ms.running set, so at most one worker works on a member.
func (r *Router) work(ms *memberState) {
	for {
		if r.ctx.Err() != nil {
			r.stopRunning(ms)
			return
		}
		ms.mu.Lock()
		if ms.canceled || !ms.backing.healthy.Load() {
			// An unhealthy backing is resumed by onBackingUp.
			ms.running = false
			ms.mu.Unlock()
			return
		}
		if ms.needFull {
			ms.needFull = false
			ms.progress = &Progress{}
			ms.mu.Unlock()
			err := r.fullSync(r.ctx, ms)
			ms.mu.Lock()
			ms.progress = nil
			if err == nil {
				ms.fullDone = true
				ms.lastSyncAt = time.Now().UTC()
			} else if !errors.Is(err, errCanceled) {
				ms.needFull = true
			}
			ms.mu.Unlock()
			if err != nil {
				r.fail(ms, err)
				return
			}
			r.succeeded(ms)
			continue
		}
		if len(ms.dirty) > 0 {
			keys := make([]string, 0, min(len(ms.dirty), syncBatch))
			for k := range ms.dirty {
				keys = append(keys, k)
				delete(ms.dirty, k)
				if len(keys) == syncBatch {
					break
				}
			}
			ms.mu.Unlock()
			for i, key := range keys {
				err := r.syncMemberKey(r.ctx, ms, key, nil)
				if errors.Is(err, copier.ErrTooManyVersions) {
					r.skipKey(ms, key, err)
					continue
				}
				if err != nil {
					ms.mu.Lock()
					for _, k := range keys[i:] {
						ms.dirty[k] = struct{}{}
					}
					ms.mu.Unlock()
					r.fail(ms, err)
					return
				}
			}
			r.succeeded(ms)
			continue
		}
		ms.mu.Unlock()
		if err := r.onIdle(ms); err != nil {
			r.fail(ms, err)
			return
		}
		ms.mu.Lock()
		if len(ms.dirty) == 0 && !ms.needFull {
			ms.running = false
			ms.mu.Unlock()
			return
		}
		ms.mu.Unlock()
	}
}

func (r *Router) stopRunning(ms *memberState) {
	ms.mu.Lock()
	ms.running = false
	ms.mu.Unlock()
}

func (r *Router) succeeded(ms *memberState) {
	ms.mu.Lock()
	ms.failures = 0
	ms.lastErr = ""
	ms.mu.Unlock()
}

// fail records a failed turn and schedules a retry with back-off.
func (r *Router) fail(ms *memberState, err error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.running = false
	if ms.canceled || errors.Is(err, errCanceled) || r.ctx.Err() != nil {
		return
	}
	ms.failures++
	ms.lastErr = err.Error()
	d := minBackoff << min(ms.failures-1, 6)
	if d > maxBackoff {
		d = maxBackoff
	}
	if ms.failures == 1 || ms.failures%10 == 0 {
		r.log.Warn("copying a bucket to a backing failed; retrying", "bucket", ms.bucket.name,
			"backing", ms.backing.Name, "failures", ms.failures, "retry_in", d, "err", err)
	}
	ms.retry = time.AfterFunc(d, func() {
		ms.mu.Lock()
		ms.retry = nil
		ms.mu.Unlock()
		r.kick(ms)
	})
}

// onIdle runs when a member has no pending changes: a replica that
// finished its initial copy is marked synced, an active-active replica
// switches to synchronous writes, and a move target takes the source's
// place. None of this happens while the member has skipped keys (it is
// not a full copy); they are retried later instead.
func (r *Router) onIdle(ms *memberState) error {
	bs := ms.bucket
	ms.mu.Lock()
	fullDone, skipped := ms.fullDone, len(ms.skipped)
	ms.mu.Unlock()
	if !fullDone {
		return nil
	}
	if skipped > 0 {
		r.retrySkipped(ms)
		return nil
	}
	r.mu.RLock()
	rec, removed, syncMode := bs.rec, bs.removed, ms.syncMode
	r.mu.RUnlock()
	if removed {
		return nil
	}
	name := ms.backing.Name
	switch {
	case rec.Move != nil && rec.Move.To == name:
		return r.cutover(ms)
	case rec.member(name) <= 0:
		return nil // the primary, or no longer a member
	}
	if !rec.Members[rec.member(name)].Synced {
		r.adminMu.Lock()
		changed := r.updateRec(bs, func(p *placementRecord) bool {
			i := p.member(name)
			if i <= 0 || p.Members[i].Synced {
				return false
			}
			p.Members[i].Synced, p.Members[i].Diverged = true, false
			return true
		})
		r.adminMu.Unlock()
		if changed {
			r.log.Info("bucket replica completed its initial copy", "bucket", bs.name, "backing", name)
			r.saveLogged("a replica completed its initial copy")
		}
	}
	if rec.Mode == ModeActiveActive && !syncMode {
		return r.enterSyncMode(ms)
	}
	return nil
}

// updateRec applies fn to a copy of the bucket's placement and publishes
// it when fn reports a change. It returns whether it did.
func (r *Router) updateRec(bs *bucketState, fn func(p *placementRecord) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if bs.removed {
		return false
	}
	next := bs.rec.clone()
	if !fn(next) {
		return false
	}
	bs.rec = next
	return true
}

// drain copies every pending change of ms while the bucket's gate is
// closed, so that no new change arrives. A key with too many versions is
// skipped (see skipKey): callers check skippedKeys afterwards.
func (r *Router) drain(ctx context.Context, ms *memberState) error {
	for {
		ms.mu.Lock()
		if ms.needFull {
			ms.mu.Unlock()
			return errors.New("a full comparison is pending")
		}
		keys := make([]string, 0, len(ms.dirty))
		for k := range ms.dirty {
			keys = append(keys, k)
		}
		ms.dirty = map[string]struct{}{}
		ms.mu.Unlock()
		if len(keys) == 0 {
			return nil
		}
		for i, key := range keys {
			err := r.syncMemberKey(ctx, ms, key, nil)
			if errors.Is(err, copier.ErrTooManyVersions) {
				r.skipKey(ms, key, err)
				continue
			}
			if err != nil {
				ms.mu.Lock()
				for _, k := range keys[i:] {
					ms.dirty[k] = struct{}{}
				}
				ms.mu.Unlock()
				return err
			}
		}
	}
}

// quiesce closes the bucket's gate for a cutover, bounded by
// CutoverTimeout.
func (r *Router) quiesce(ctx context.Context, bs *bucketState) (func(), error) {
	qctx, cancel := context.WithTimeout(ctx, r.opts.CutoverTimeout)
	defer cancel()
	release, err := bs.gate.quiesce(qctx)
	if err != nil {
		return nil, fmt.Errorf("waiting for the writes in flight on bucket %q: %w", bs.name, err)
	}
	return release, nil
}

// enterSyncMode switches an active-active replica with no pending changes
// to synchronous writes.
func (r *Router) enterSyncMode(ms *memberState) error {
	bs := ms.bucket
	release, err := r.quiesce(r.ctx, bs)
	if err != nil {
		return err
	}
	defer release()
	if err := r.drain(r.ctx, ms); err != nil {
		return err
	}
	if ms.skippedKeys() > 0 {
		r.retrySkipped(ms) // a key changed meanwhile and cannot be copied
		return nil
	}
	r.mu.Lock()
	ok := !bs.removed && bs.rec.Mode == ModeActiveActive && bs.rec.member(ms.backing.Name) > 0 &&
		ms.backing.healthy.Load() && !ms.isCanceled()
	if ok {
		ms.syncMode = true
	}
	r.mu.Unlock()
	if ok {
		r.log.Info("active-active replica is in sync and receives writes synchronously",
			"bucket", bs.name, "backing", ms.backing.Name)
	}
	return nil
}

func (ms *memberState) isCanceled() bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.canceled
}

// cutover completes a move: with the bucket's gate closed, the target
// receives the last changes and takes the source's place; the source's
// copy is then deleted.
func (r *Router) cutover(target *memberState) error {
	bs := target.bucket
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	r.mu.Lock()
	mv := bs.rec.Move
	if bs.removed || mv == nil || mv.To != target.backing.Name {
		r.mu.Unlock()
		return nil
	}
	bs.movePh = phaseCutover
	r.mu.Unlock()
	reset := func() {
		r.mu.Lock()
		if bs.rec.Move != nil {
			bs.movePh = phaseCopying
		}
		r.mu.Unlock()
	}

	release, err := r.quiesce(r.ctx, bs)
	if err != nil {
		reset()
		return err
	}
	defer release()
	if err := r.drain(r.ctx, target); err != nil {
		reset()
		return err
	}
	if target.skippedKeys() > 0 {
		// A key changed meanwhile and cannot be copied: the move waits.
		reset()
		r.retrySkipped(target)
		return nil
	}
	var source *memberState
	r.mu.Lock()
	next := bs.rec.clone()
	i := next.member(mv.From)
	if i < 0 {
		r.mu.Unlock()
		reset()
		return fmt.Errorf("move source %q is no longer a member", mv.From)
	}
	wasPrimary := i == 0
	diverged := next.Members[i].Diverged
	source = bs.members[mv.From]
	next.Members[i] = memberRecord{Backing: mv.To, Synced: true}
	next.Move = nil
	bs.rec = next
	bs.movePh = ""
	delete(bs.members, mv.From)
	// The target is identical to the source now: in an active-active
	// bucket it takes over the source's synchronous writes.
	target.syncMode = next.Mode == ModeActiveActive && !wasPrimary
	if wasPrimary {
		// Replicas were copied from the old primary; pending keys are
		// copied from the new one from now on (identical content).
		target.syncMode = false
	}
	r.addPurgeLocked(bs.name, mv.From, diverged)
	r.mu.Unlock()
	if source != nil {
		r.cancelMember(source)
	}
	r.log.Info("bucket move completed", "bucket", bs.name, "from", mv.From, "to", mv.To,
		"took", time.Since(mv.StartedAt).Round(time.Second))
	r.saveLogged("a bucket move")
	r.mu.RLock()
	r.startPurge(r.purges[purgeRecord{Bucket: bs.name, Backing: mv.From}])
	r.mu.RUnlock()
	return nil
}

// failover promotes the first usable replica of a bucket whose primary's
// backing (down) failed. Active-active buckets promote an in-sync replica;
// active-passive ones (with AutoFailover) a synced replica, losing the
// changes not yet copied to it.
func (r *Router) failover(bs *bucketState, down string) {
	// Lock order: adminMu, then the gate (as cutover and Promote).
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	ctx, cancel := context.WithTimeout(r.ctx, r.opts.CutoverTimeout)
	defer cancel()
	// Wait (briefly) for the writes in flight, which are failing anyway;
	// proceed without the gate when they do not finish.
	release, err := bs.gate.quiesce(ctx)
	if err == nil {
		defer release()
	}
	r.mu.Lock()
	rec := bs.rec
	if bs.removed || r.deleting[bs.name] || rec.primary() != down || r.topology.Load().backings[down].healthy.Load() {
		r.mu.Unlock()
		return
	}
	pick := -1
	for i := 1; i < len(rec.Members); i++ {
		ms := bs.members[rec.Members[i].Backing]
		// A replica with skipped keys lacks their latest versions, even if
		// it was synced before they were skipped.
		if ms == nil || !ms.backing.healthy.Load() || !rec.Members[i].Synced || ms.skippedKeys() > 0 {
			continue
		}
		if rec.Mode == ModeActiveActive && !ms.syncMode {
			continue
		}
		pick = i
		break
	}
	if pick < 0 {
		r.mu.Unlock()
		r.log.Error("the primary backing of a bucket failed and no replica can take over",
			"bucket", bs.name, "backing", down, "mode", rec.Mode)
		return
	}
	newPrimary := rec.Members[pick].Backing
	r.promoteLocked(bs, pick, true)
	r.mu.Unlock()
	r.log.Warn("failed over a bucket to a replica", "bucket", bs.name, "failed_primary", down,
		"new_primary", newPrimary, "mode", rec.Mode)
	r.saveLogged("a failover")
}

// promoteLocked makes member i the primary. forced: the other members may
// differ from the new primary (they get a full comparison); otherwise they
// are known to be identical (graceful promote after a drain). Router.mu
// must be held.
func (r *Router) promoteLocked(bs *bucketState, i int, forced bool) {
	next := bs.rec.clone()
	old := next.Members[0]
	next.Members[0], next.Members[i] = next.Members[i], next.Members[0]
	next.Members[0].Synced, next.Members[0].Diverged = true, false
	if forced {
		for j := 1; j < len(next.Members); j++ {
			next.Members[j].Synced = false
			next.Members[j].Diverged = true
		}
	} else {
		next.Members[i] = memberRecord{Backing: old.Backing, Synced: true}
	}
	bs.rec = next
	newPrimary := bs.members[next.Members[0].Backing]
	newPrimary.syncMode = false
	newPrimary.mu.Lock()
	newPrimary.dirty = map[string]struct{}{}
	newPrimary.skipped = map[string]error{}
	newPrimary.nskipped.Store(0)
	newPrimary.needFull = false
	newPrimary.mu.Unlock()
	oldPrimary := bs.members[old.Backing]
	oldPrimary.syncMode = !forced && next.Mode == ModeActiveActive
	for _, m := range next.Members[1:] {
		ms := bs.members[m.Backing]
		if forced {
			ms.syncMode = false
			ms.mu.Lock()
			ms.needFull = true
			ms.fullDone = false
			ms.mu.Unlock()
		}
		go r.kick(ms)
	}
}
