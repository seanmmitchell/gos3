package multi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
)

// errWriterDone is returned when using a writer that was committed or
// aborted.
var errWriterDone = errors.New("multi: writer already committed or aborted")

// checkCompression refuses compression options that no backing accepts
// (the rules of the memory and fs backings) before any backing sees them:
// a backing's refusal would be taken for a failure of that backing.
func checkCompression(c storage.Compression) error {
	switch {
	case !c.Enabled():
		return nil
	case c.Algorithm != storage.CompressionDeflate:
		return fmt.Errorf("multi: unsupported compression algorithm %q", c.Algorithm)
	case !compress.ValidLevel(c.Level):
		return fmt.Errorf("multi: invalid deflate level %d", c.Level)
	}
	return nil
}

// withOptions reports whether a write needs the write-options variant of a
// backing's methods (storage.CompressingBackend, CompressingImporter):
// only then, so that backings without them still take plain writes.
func withOptions(o storage.WriteOptions) bool {
	return o.Compression.Enabled() || o.Encryption != ""
}

// errNoWriteOptions is the error for the backing name ("" for the
// bucket's primary) that cannot store data as the write options o ask.
func errNoWriteOptions(name string, o storage.WriteOptions) error {
	what := "compressed"
	if o.Encryption != "" {
		what = "encrypted (" + o.Encryption + ")"
	}
	who := "the primary backing"
	if name != "" {
		who = fmt.Sprintf("backing %q", name)
	}
	return fmt.Errorf("%s cannot store data %s: %w", who, what, errors.ErrUnsupported)
}

// leadWriter is the primary's writer of a fanout: a storage.ObjectWriter
// or a storage.VersionWriter, whose Commit methods differ.
type leadWriter interface {
	io.Writer
	Abort() error
}

// fanout is what objectWriter and versionWriter share: it streams a new
// version to the bucket's primary (lead) and, in an active-active bucket,
// the same bytes to every in-sync replica (tees), which stores the version
// under the identity the primary committed. It holds the bucket's gate
// from creation until commit or Abort, so that no cutover happens while
// the version is in flight.
type fanout struct {
	r      *Router
	rt     *route
	bucket string
	key    string
	lead   leadWriter
	tees   []*teeWriter

	mu    sync.Mutex
	done  bool
	leave func()
}

// teeWriter is one replica's copy of the stream.
type teeWriter struct {
	ms     *memberState
	w      storage.VersionWriter
	failed bool
}

// newFanout starts the tees of a write of bucket/key along rt, whose lead
// writer is lead and whose gate leave releases: one imported version per
// follower, stored as opts asks. A follower that cannot take the version
// is demoted (the key is copied to it in the background).
func (r *Router) newFanout(ctx context.Context, rt *route, leave func(), bucket, key string, lead leadWriter, opts storage.WriteOptions) *fanout {
	f := &fanout{r: r, rt: rt, bucket: bucket, key: key, lead: lead, leave: leave}
	for _, m := range rt.followers {
		vw, err := newVersionWriter(ctx, m.backing, bucket, key, opts)
		if err != nil {
			r.observe(m.backing, err) // errors.ErrUnsupported is not a failure of the backing
			r.demote(m, key)
			continue
		}
		f.tees = append(f.tees, &teeWriter{ms: m, w: vw})
	}
	return f
}

func (f *fanout) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return 0, errWriterDone
	}
	n, err := f.lead.Write(p)
	if err != nil {
		f.r.observe(f.rt.lead, err)
		return n, err
	}
	for _, t := range f.tees {
		if t.failed {
			continue
		}
		if _, err := t.w.Write(p[:n]); err != nil {
			t.failed = true
			_ = t.w.Abort()
			f.r.observe(t.ms.backing, err)
		}
	}
	return n, nil
}

// commit publishes the version: commitLead commits the lead writer and
// returns the stored info, with which every tee is committed; cb (may be
// nil) tells the errors of the caller's callbacks apart. The change is
// reported to the subscribers (see Subscribe) once the gate and key lock
// are released.
func (f *fanout) commit(ctx context.Context, cb *cbTracker, commitLead func() (storage.ObjectInfo, error)) (storage.ObjectInfo, error) {
	info, err := f.commitGated(ctx, cb, commitLead)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	f.r.emitObject(f.bucket, f.key)
	return info, nil
}

func (f *fanout) commitGated(ctx context.Context, cb *cbTracker, commitLead func() (storage.ObjectInfo, error)) (storage.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return storage.ObjectInfo{}, errWriterDone
	}
	f.done = true
	defer f.leave()
	if len(f.tees) > 0 || len(f.rt.followers) > 0 {
		// Replicas must commit the key's versions in the primary's order.
		unlock := f.r.keys.lock(f.bucket, f.key)
		defer unlock()
	}
	info, err := commitLead()
	if err != nil {
		// Abort after Commit is a no-op; it frees what a failed commit may
		// have kept, since the caller's Abort is now ours to forward.
		_ = f.lead.Abort()
		f.abortTees()
		if !cb.isCaller(err) {
			f.r.observe(f.rt.lead, err)
		}
		return storage.ObjectInfo{}, err
	}
	committed := map[*memberState]bool{}
	for _, t := range f.tees {
		if t.failed {
			continue
		}
		if _, err := t.w.Commit(ctx, info); err != nil {
			f.r.observe(t.ms.backing, err)
			continue
		}
		committed[t.ms] = true
	}
	f.r.settle(f.rt, f.key, committed)
	return info, nil
}

// Abort discards the version on every backing.
func (f *fanout) Abort() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return nil
	}
	f.done = true
	defer f.leave()
	f.abortTees()
	return f.lead.Abort()
}

// abortTees aborts the tees still writing. f.mu must be held.
func (f *fanout) abortTees() {
	for _, t := range f.tees {
		if !t.failed {
			_ = t.w.Abort()
		}
	}
}

// settle propagates a change of key, made on rt.lead, to the members that
// did not take it synchronously: a follower missing from committed is
// demoted, one demoted meanwhile by another failure gets the key checked
// in the background (it may have missed something since), and so does
// every other member (see markAsync). The caller still holds the bucket's
// gate.
func (r *Router) settle(rt *route, key string, committed map[*memberState]bool) {
	for _, f := range rt.followers {
		switch {
		case !committed[f]:
			r.demote(f, key)
		case !r.inSyncMode(f):
			r.markDirty(f, key)
		}
	}
	r.markAsync(rt, key)
}

// markAsync queues key for the members of rt's bucket that copy changes in
// the background. They are taken from the bucket's current route, not from
// rt (computed when the write started): a replica or move target added
// meanwhile (SetReplication and Move do not wait for the writes in
// flight) may have compared the key before it changed, and would then miss
// the change. Followers cannot join while the caller holds the gate (that
// takes a cutover).
func (r *Router) markAsync(rt *route, key string) {
	for _, a := range r.writeRoute(rt.bs).async {
		r.markDirty(a, key)
	}
}

// objectWriter writes a new version to the bucket's primary and, in an
// active-active bucket, streams the same bytes to every in-sync replica,
// which stores the version under the ID the primary assigned at commit
// (see fanout).
type objectWriter struct {
	*fanout
	w storage.ObjectWriter
}

// NewObjectWriter starts writing a new version of bucket/key.
func (r *Router) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	return r.NewObjectWriterWith(ctx, bucket, key, storage.WriteOptions{})
}

// NewObjectWriterWith implements storage.CompressingBackend: the primary,
// and in an active-active bucket every in-sync replica, store the version
// as opts asks.
func (r *Router) NewObjectWriterWith(ctx context.Context, bucket, key string, opts storage.WriteOptions) (storage.ObjectWriter, error) {
	if err := checkCompression(opts.Compression); err != nil {
		return nil, err
	}
	if err := r.checkWriteEncryption(ctx, bucket, opts); err != nil {
		return nil, err
	}
	rt, leave, err := r.enterWrite(ctx, bucket)
	if err != nil {
		return nil, err
	}
	var w storage.ObjectWriter
	if withOptions(opts) {
		cb, ok := rt.lead.Backend.(storage.CompressingBackend)
		if !ok {
			leave()
			return nil, errNoWriteOptions(rt.lead.Name, opts)
		}
		w, err = cb.NewObjectWriterWith(ctx, bucket, key, opts)
	} else {
		w, err = rt.lead.Backend.NewObjectWriter(ctx, bucket, key)
	}
	if err != nil {
		leave()
		return nil, r.observe(rt.lead, err)
	}
	return &objectWriter{fanout: r.newFanout(ctx, rt, leave, bucket, key, w, opts), w: w}, nil
}

// Commit publishes the version on the primary, then on the tees, under the
// ID the primary assigned.
func (w *objectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	cb := &cbTracker{}
	return w.commit(ctx, cb, func() (storage.ObjectInfo, error) {
		return w.w.Commit(ctx, meta, cb.commitOptions(opts))
	})
}
