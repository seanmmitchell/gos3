package copyjob

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// maxDirty is the most dirty keys a mirror remembers; beyond it the next
// run makes a full pass instead.
const maxDirty = 100_000

// watch is an active mirror's view of the router's changes, kept in
// memory: the source keys changed since the job last copied them, and
// whether the source's configuration changed or a bucket was deleted. It
// also remembers what this process knows of the job's passes.
type watch struct {
	id       string
	src, dst keyRange

	mu sync.Mutex
	// dirty maps changed source keys to when they first changed.
	dirty map[string]time.Time
	// overflow: more than maxDirty keys changed; a full pass is due.
	overflow bool
	// configDirty: the source's configuration (or versioning) changed.
	configDirty bool
	// deleted names the source or target bucket that was deleted.
	deleted, deletedRole string
	// timer is the pending debounced kick.
	timer *time.Timer
	// live: the checkpoint of the current pass was written by this process,
	// which has been watching changes all along, so resuming the pass
	// misses nothing. verified: a full pass completed with live set.
	live, verified bool
}

// watchIndex holds the watches by bucket (copy-on-write).
type watchIndex struct {
	bySrc, byDst map[string][]*watch
}

// register returns the watch of mirror job id, creating it.
func (s *Service) register(id string, sp Spec) *watch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.watches[id]; w != nil {
		return w
	}
	w := &watch{id: id, src: sp.srcRange(), dst: sp.dstRange(), dirty: map[string]time.Time{}}
	s.watches[id] = w
	s.reindexLocked()
	return w
}

// unregister drops the watch of job id.
func (s *Service) unregister(id string) {
	s.mu.Lock()
	w := s.watches[id]
	if w != nil {
		delete(s.watches, id)
		s.reindexLocked()
	}
	s.mu.Unlock()
	if w != nil {
		w.stopTimer()
	}
}

// reindexLocked rebuilds the bucket index of the watches (s.mu held).
func (s *Service) reindexLocked() {
	idx := &watchIndex{bySrc: map[string][]*watch{}, byDst: map[string][]*watch{}}
	for _, w := range s.watches {
		idx.bySrc[w.src.bucket] = append(idx.bySrc[w.src.bucket], w)
		idx.byDst[w.dst.bucket] = append(idx.byDst[w.dst.bucket], w)
	}
	s.index.Store(idx)
}

// onChange is the router's change notification (multi.Router.Subscribe):
// it records the change in the watches it concerns and schedules their
// kicks. It does not block and does not call the router.
func (s *Service) onChange(c multi.Change) {
	idx := s.index.Load()
	if idx == nil {
		return
	}
	switch c.Kind {
	case multi.ChangeObject:
		for _, w := range idx.bySrc[c.Bucket] {
			if strings.HasPrefix(c.Key, w.src.prefix) && w.markKey(c.Key, time.Now()) {
				s.schedule(w)
			}
		}
	case multi.ChangeBucketConfig:
		for _, w := range idx.bySrc[c.Bucket] {
			w.mu.Lock()
			w.configDirty = true
			w.mu.Unlock()
			s.schedule(w)
		}
	case multi.ChangeBucketDeleted:
		for _, w := range idx.bySrc[c.Bucket] {
			w.markDeleted(c.Bucket, "source")
			s.schedule(w)
		}
		for _, w := range idx.byDst[c.Bucket] {
			w.markDeleted(c.Bucket, "target")
			s.schedule(w)
		}
	}
}

// schedule kicks w's job after the debounce delay, unless a kick is
// pending already.
func (s *Service) schedule(w *watch) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		return
	}
	w.timer = time.AfterFunc(s.debounce, func() { s.fire(w) })
}

// fire kicks w's job (see schedule). A job that is gone or finished loses
// its watch.
func (s *Service) fire(w *watch) {
	w.mu.Lock()
	w.timer = nil
	w.mu.Unlock()
	m := s.mgr.Load()
	if m == nil {
		return // Attach kicks every mirror
	}
	j, err := m.Get(w.id)
	if err != nil || j.State.Finished() {
		s.unregister(w.id)
		return
	}
	m.Kick(w.id)
}

func (w *watch) stopTimer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// markKey records a change of source key; false when nothing new is
// recorded (a full pass is due anyway).
func (w *watch) markKey(key string, at time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.overflow {
		return true
	}
	if _, ok := w.dirty[key]; ok {
		return true
	}
	if len(w.dirty) >= maxDirty {
		w.overflow = true
		w.dirty = map[string]time.Time{}
		return true
	}
	w.dirty[key] = at
	return true
}

func (w *watch) markDeleted(bucket, role string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.deleted == "" {
		w.deleted, w.deletedRole = bucket, role
	}
}

// deletedBucket returns the deleted bucket and its role ("" when none).
func (w *watch) deletedBucket() (string, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deleted, w.deletedRole
}

// takeConfig reports and clears a pending configuration change.
func (w *watch) takeConfig() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.configDirty
	w.configDirty = false
	return c
}

// takeOverflow reports and clears an overflow of the dirty set (a full
// pass covers the keys).
func (w *watch) takeOverflow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	o := w.overflow
	w.overflow = false
	return o
}

// overflowed reports a pending overflow without clearing it.
func (w *watch) overflowed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overflow
}

// takeDirty returns the dirty keys, sorted, with when they first changed,
// and clears them.
func (w *watch) takeDirty() ([]string, map[string]time.Time) {
	w.mu.Lock()
	d := w.dirty
	w.dirty = map[string]time.Time{}
	w.mu.Unlock()
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, d
}

// requeue marks keys dirty again (keys that failed), keeping when they
// first changed.
func (w *watch) requeue(keys []string, since map[string]time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, k := range keys {
		if w.overflow {
			return
		}
		if _, ok := w.dirty[k]; ok {
			continue
		}
		if len(w.dirty) >= maxDirty {
			w.overflow = true
			w.dirty = map[string]time.Time{}
			return
		}
		at := since[k]
		if at.IsZero() {
			at = time.Now()
		}
		w.dirty[k] = at
	}
}

// pending returns the number of dirty keys and when the oldest changed
// (zero when none).
func (w *watch) pending() (int, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var oldest time.Time
	for _, at := range w.dirty {
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	return len(w.dirty), oldest
}

func (w *watch) setLive(live bool) {
	w.mu.Lock()
	w.live = live
	w.mu.Unlock()
}

func (w *watch) isLive() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.live
}

func (w *watch) setVerified() {
	w.mu.Lock()
	w.verified = true
	w.mu.Unlock()
}

func (w *watch) isVerified() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.verified
}
