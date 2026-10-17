package multi

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// ChangeKind says what a Change is about.
type ChangeKind uint8

// The kinds of Change.
const (
	// ChangeObject: a version of Change.Key was written (an object write,
	// an imported version, a completed multipart upload), deleted (or a
	// delete marker created), or had its metadata changed.
	ChangeObject ChangeKind = iota + 1
	// ChangeBucketConfig: the bucket configuration document
	// Change.ConfigKey (one of the storage.Config* keys) was stored or
	// deleted, or, with ConfigKey "", a bucket setting reported by
	// GetBucket (versioning, object lock) changed.
	ChangeBucketConfig
	// ChangeBucketCreated: the bucket was created.
	ChangeBucketCreated
	// ChangeBucketDeleted: the bucket was deleted, or released
	// (ReleaseBucket): either way the router no longer has it.
	ChangeBucketDeleted
)

// String returns the kind's name (for logs).
func (k ChangeKind) String() string {
	switch k {
	case ChangeObject:
		return "object"
	case ChangeBucketConfig:
		return "bucket-config"
	case ChangeBucketCreated:
		return "bucket-created"
	case ChangeBucketDeleted:
		return "bucket-deleted"
	}
	return fmt.Sprintf("ChangeKind(%d)", uint8(k))
}

// Change describes a change made through the Router (see Subscribe).
type Change struct {
	Kind   ChangeKind
	Bucket string
	// Key is the object key of a ChangeObject ("" otherwise).
	Key string
	// ConfigKey is the configuration key of a ChangeBucketConfig ("" for a
	// bucket setting: versioning, object lock).
	ConfigKey string
}

// subscription is one Subscribe call.
type subscription struct {
	fn  func(Change)
	off atomic.Bool // unsubscribed
}

// changeFeed holds a Router's subscribers, copy-on-write: emit reads the
// current list without locking, Subscribe and unsubscribing replace it
// under mu.
type changeFeed struct {
	mu   sync.Mutex
	subs atomic.Pointer[[]*subscription]
}

// Subscribe calls fn with every change made through the router once the
// change has been acknowledged (see Change and ChangeKind): object writes,
// imported versions, completed multipart uploads, deletes and metadata
// updates, bucket settings and configuration documents, and the creation
// and deletion of buckets. Failed operations report nothing, and neither
// does the router's own copying between backings (replicas, moves), which
// changes no bucket as clients see it, nor anything that changes a backing
// behind the router's back (another tool writing to a native bucket's
// remote bucket).
//
// Changes are hints, not a log: a subscriber that needs the current state
// reads it again (a write and its event are not atomic, and the events of
// concurrent writes may arrive in any order). A change acknowledged to the
// caller has been reported when the router's method returns.
//
// fn is called synchronously on the goroutine of the operation, after the
// router released its locks; it must not block and must not call the
// Router (hand the change to another goroutine, e.g. through a buffered
// channel or a dirty set). unsubscribe stops the calls; a call already
// under way on another goroutine may still finish after it returns.
// unsubscribe may be called more than once.
func (r *Router) Subscribe(fn func(Change)) (unsubscribe func()) {
	if fn == nil {
		return func() {}
	}
	s := &subscription{fn: fn}
	f := &r.changes
	f.mu.Lock()
	next := append(slices.Clone(f.current()), s)
	f.subs.Store(&next)
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.off.Store(true)
			f.mu.Lock()
			defer f.mu.Unlock()
			next := slices.DeleteFunc(slices.Clone(f.current()), func(x *subscription) bool { return x == s })
			f.subs.Store(&next)
		})
	}
}

// current returns the current subscribers (read-only).
func (f *changeFeed) current() []*subscription {
	if p := f.subs.Load(); p != nil {
		return *p
	}
	return nil
}

// emit reports c to the subscribers. Callers emit once the change is
// acknowledged, after releasing the router's locks (the gate, key locks,
// adminMu).
func (r *Router) emit(c Change) {
	for _, s := range r.changes.current() {
		if !s.off.Load() {
			s.fn(c)
		}
	}
}

// emitObject reports a change of bucket/key.
func (r *Router) emitObject(bucket, key string) {
	r.emit(Change{Kind: ChangeObject, Bucket: bucket, Key: key})
}
