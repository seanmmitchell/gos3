package multi

import (
	"context"
	"errors"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// errWriterDone is returned when using a writer that was committed or
// aborted.
var errWriterDone = errors.New("multi: writer already committed or aborted")

// objectWriter writes a new version to the bucket's primary and, in an
// active-active bucket, streams the same bytes to every in-sync replica,
// which stores the version under the ID the primary assigned at commit.
// It holds the bucket's gate from creation until Commit or Abort.
type objectWriter struct {
	r      *Router
	rt     *route
	bucket string
	key    string
	w      storage.ObjectWriter
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

// NewObjectWriter starts writing a new version of bucket/key.
func (r *Router) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	rt, leave, err := r.enterWrite(ctx, bucket)
	if err != nil {
		return nil, err
	}
	w, err := rt.lead.Backend.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		leave()
		return nil, r.observe(rt.lead, err)
	}
	ow := &objectWriter{r: r, rt: rt, bucket: bucket, key: key, w: w, leave: leave}
	for _, f := range rt.followers {
		imp, ok := f.backing.Backend.(storage.VersionImporter)
		if !ok {
			r.demote(f, key)
			continue
		}
		vw, err := imp.NewVersionWriter(ctx, bucket, key)
		if err != nil {
			r.observe(f.backing, err)
			r.demote(f, key)
			continue
		}
		ow.tees = append(ow.tees, &teeWriter{ms: f, w: vw})
	}
	return ow, nil
}

func (w *objectWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	n, err := w.w.Write(p)
	if err != nil {
		w.r.observe(w.rt.lead, err)
		return n, err
	}
	for _, t := range w.tees {
		if t.failed {
			continue
		}
		if _, err := t.w.Write(p[:n]); err != nil {
			t.failed = true
			_ = t.w.Abort()
			w.r.observe(t.ms.backing, err)
		}
	}
	return n, nil
}

// finish marks the writer done and leaves the gate (once).
func (w *objectWriter) finish() bool {
	if w.done {
		return false
	}
	w.done = true
	return true
}

func (w *objectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.finish() {
		return storage.ObjectInfo{}, errWriterDone
	}
	defer w.leave()
	if len(w.tees) > 0 || len(w.rt.followers) > 0 {
		// Replicas must commit the key's versions in the primary's order.
		unlock := w.r.keys.lock(w.bucket, w.key)
		defer unlock()
	}
	info, err := w.w.Commit(ctx, meta, opts)
	if err != nil {
		for _, t := range w.tees {
			if !t.failed {
				_ = t.w.Abort()
			}
		}
		w.r.observe(w.rt.lead, err)
		return storage.ObjectInfo{}, err
	}
	committed := map[*memberState]bool{}
	for _, t := range w.tees {
		if t.failed {
			continue
		}
		if _, err := t.w.Commit(ctx, info); err != nil {
			w.r.observe(t.ms.backing, err)
			continue
		}
		committed[t.ms] = true
	}
	for _, f := range w.rt.followers {
		switch {
		case !committed[f]:
			w.r.demote(f, w.key)
		case !w.r.inSyncMode(f):
			// Demoted meanwhile by another failure: it may have missed
			// something since; let the background copy check the key.
			w.r.markDirty(f, w.key)
		}
	}
	for _, a := range w.rt.async {
		w.r.markDirty(a, w.key)
	}
	return info, nil
}

func (w *objectWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.finish() {
		return nil
	}
	defer w.leave()
	for _, t := range w.tees {
		if !t.failed {
			_ = t.w.Abort()
		}
	}
	return w.w.Abort()
}
