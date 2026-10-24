package s3account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// Containers
//
// A container is an s3store (a gos3 store at a remote bucket and prefix)
// holding wrapped buckets. The home is the implicit container "home"; the
// others are registered by k/ records and opened when the registry loads
// (MustExist with the recorded backing ID, so a swapped location is never
// used) or by AddContainer. Each loads in the background; until it is
// loaded its buckets answer storage.ErrBucketUnavailable (503) and are
// listed from the registry's cached info.
//
// Errors of a container other than the home are classified (classify):
// failures of the remote service (transport errors, 5xx answers) stay
// backing-level (storage.ErrBackingUnavailable: the whole service is
// down), anything else that keeps the container from serving (not loaded
// yet, in use by another server, a wrong KEK or marker, a deleted bucket,
// 403 answers, ...) only affects the container's buckets and wraps
// storage.ErrBucketUnavailable, so the multi router does not mark the
// backing down. The home's errors are the backing's.

// maxContainers is the documented soft cap on the containers of a backing
// (the home included): each container is a loaded s3store with its own
// lease, journal and background work.
const maxContainers = 64

// container is a loaded (or loading) container.
type container struct {
	id   string
	rec  containerRecord // the home's is synthesized
	home bool
	// store is nil when the container could not be opened (failErr).
	store *s3store.Backend
	// failErr is why the container cannot be used: its location is
	// claimed by another use, or its options are invalid.
	failErr error
	// release forgets the container's claim (nil for the home, whose
	// claim the Backend holds).
	release func()
	// conn is the connection the store was opened or last switched with
	// (adminMu, or before the container is published): PrepareConnection's
	// commit switches every container whose conn is not the backing's.
	conn *s3store.Connection
	// done is closed when the container is closed (ends its watcher).
	done chan struct{}
}

// location returns the container's remote location.
func (c *container) location() Location { return c.rec.location() }

// locationString returns "s3://bucket/prefix".
func (c *container) locationString() string { return c.location().String() }

// state returns the container's state for ContainerInfo: "ready",
// "loading" or "failed", and the failure.
func (c *container) state() (string, string) {
	if c.store == nil {
		return "failed", errString(c.failErr)
	}
	st := c.store.Status()
	switch st.State {
	case s3store.StateServing:
		return "ready", ""
	case s3store.StateFailed, s3store.StateClosed:
		return "failed", errString(st.Err)
	}
	if st.Err != nil {
		return "loading", st.Err.Error()
	}
	return "loading", ""
}

// loaded reports whether the container's store serves.
func (c *container) loaded() bool {
	return c.store != nil && c.store.Status().State == s3store.StateServing
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// unusable is the error of operations on the buckets of a container that
// could not be opened.
func (c *container) unusable() error {
	return fmt.Errorf("s3account: container %s (%s) cannot be used: %w: %w", c.id, c.locationString(), storage.ErrBucketUnavailable, c.failErr)
}

// classify maps an error of the container's store (see the comment at the
// top of the file); the home's errors are returned unchanged.
func (c *container) classify(err error) error {
	if err == nil || c.home {
		return err
	}
	return classifyContainerErr(c.id, err)
}

// classifyContainerErr implements container.classify for container id.
func classifyContainerErr(id string, err error) error {
	switch {
	case errors.Is(err, storage.ErrBucketUnavailable), errors.Is(err, storage.ErrResourceLimit),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	var ce *s3client.Error
	if errors.As(err, &ce) {
		if s3client.IsUnavailable(err) && !s3client.IsNotImplemented(err) {
			return err // the service is down (backing-level)
		}
		return fmt.Errorf("s3account: container %s: %w: %w", id, storage.ErrBucketUnavailable, err)
	}
	for _, e := range []error{
		storage.ErrBackingUnavailable, s3store.ErrInUse, s3store.ErrWrongKEK, s3store.ErrMarkerMismatch,
		s3store.ErrUnmanagedPrefix, s3store.ErrPrefixNotEmpty, s3store.ErrNoConditionalWrites,
		s3store.ErrCorruptJournal, s3store.ErrFenced,
	} {
		if errors.Is(err, e) {
			return fmt.Errorf("s3account: container %s: %w: %w", id, storage.ErrBucketUnavailable, err)
		}
	}
	return err
}

// storeOptions returns the s3store options of a store at bucket/prefix in
// region, using the connection conn. tempName is the name of the store's
// temporary directory in Options.TempDir. It is derived from a container
// ID (validContainerID), and anything but a single local path element is
// refused, so the directory is never created outside Options.TempDir.
func (b *Backend) storeOptions(conn *s3store.Connection, bucket, prefix, region, role string, create s3store.CreateMode, expectID, tempName string) (s3store.Options, error) {
	if !filepath.IsLocal(tempName) || strings.ContainsAny(tempName, `/\`) {
		return s3store.Options{}, fmt.Errorf("s3account: invalid temporary directory name %q", tempName)
	}
	signRegion := region
	if signRegion == "" {
		signRegion = conn.Region
	}
	o := s3store.Options{
		Name:            b.name,
		Client:          conn.Client.ForRegion(region),
		Endpoint:        conn.Endpoint,
		Region:          signRegion,
		AccessKey:       conn.AccessKey,
		SecretKey:       conn.SecretKey,
		PathStyle:       conn.PathStyle,
		Bucket:          bucket,
		Prefix:          prefix,
		PartSize:        conn.PartSize,
		KEK:             conn.KEK,
		Create:          create,
		ExpectBackingID: expectID,
		Role:            role,
		Logger:          b.log,
	}
	if b.opts.TempDir != "" {
		o.TempDir = filepath.Join(b.opts.TempDir, tempName)
	}
	if b.opts.Tune != nil {
		b.opts.Tune(&o)
	}
	return o, nil
}

// openStore opens an s3store with o, creating its temporary directory.
func openStore(o s3store.Options) (*s3store.Backend, error) {
	if o.TempDir != "" {
		if err := os.MkdirAll(o.TempDir, 0o700); err != nil {
			return nil, fmt.Errorf("s3account: create temporary directory: %w", err)
		}
	}
	return s3store.Open(o)
}

// openContainer claims rec's location and opens its store (no remote I/O).
// A container that cannot be claimed or opened is returned with failErr
// set (the caller logs it). A non-nil release is a claim the caller
// already holds for the location, which the container takes over.
func (b *Backend) openContainer(rec containerRecord, create s3store.CreateMode, expectID string, release func()) *container {
	conn := b.conn.Load()
	c := &container{id: rec.ID, rec: rec, done: make(chan struct{}), release: release, conn: conn}
	if c.release == nil {
		rel, err := b.claim(rec.location(), RoleContainer, "")
		if err != nil {
			c.failErr = err
			return c
		}
		c.release = rel
	}
	o, err := b.storeOptions(conn, rec.Bucket, rec.Prefix, rec.Region, s3store.RoleContainer, create, expectID, "c-"+rec.ID)
	var st *s3store.Backend
	if err == nil {
		st, err = openStore(o)
	}
	if err != nil {
		c.failErr = err
		c.release()
		c.release = nil
		return c
	}
	c.store = st
	return c
}

// closeContainer closes c's store and releases its claim.
func closeContainer(c *container) error {
	select {
	case <-c.done:
		return nil
	default:
	}
	close(c.done)
	var err error
	if c.store != nil {
		err = c.store.Close()
	}
	if c.release != nil {
		c.release()
	}
	return err
}

// watchContainer reconciles c once its store has loaded (see reconcile.go).
func (b *Backend) watchContainer(c *container) {
	if c.store == nil || c.home {
		return
	}
	b.spawn(func() {
		select {
		case <-c.store.Ready():
		case <-c.done:
			return
		case <-b.ctx.Done():
			return
		}
		b.adminMu.Lock()
		defer b.adminMu.Unlock()
		select {
		case <-c.done:
			return // closed meanwhile
		default:
		}
		if err := b.reconcileContainer(b.ctx, c); err != nil && b.ctx.Err() == nil {
			b.log.Warn("s3account: reconciling a container failed (retried at the next start)", "backing", b.name,
				"container", c.id, "location", c.locationString(), "err", err)
		}
	})
}

// --- wrappers classifying the errors of a container's objects ---

// callbackErr marks an error returned by a caller's callback, so it is
// returned unchanged rather than classified as the container's.
type callbackErr struct{ err error }

func (e *callbackErr) Error() string { return e.err.Error() }
func (e *callbackErr) Unwrap() error { return e.err }

// markCallback wraps fn so its errors (other than storage.ErrStopWalk) are
// recognized by unmarkCallback.
func markCallback[T any](fn func(T) error) func(T) error {
	return func(v T) error {
		err := fn(v)
		if err == nil || errors.Is(err, storage.ErrStopWalk) {
			return err
		}
		return &callbackErr{err}
	}
}

// walkErr returns a callback's error unchanged and classifies the rest.
func (c *container) walkErr(err error) error {
	var cb *callbackErr
	if errors.As(err, &cb) {
		return cb.err
	}
	return c.classify(err)
}

// objectWriter classifies the errors of a container's object writer.
type objectWriter struct {
	storage.ObjectWriter
	c *container
}

func (w *objectWriter) Write(p []byte) (int, error) {
	n, err := w.ObjectWriter.Write(p)
	return n, w.c.classify(err)
}

func (w *objectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	info, err := w.ObjectWriter.Commit(ctx, meta, opts)
	return info, w.c.classify(err)
}

// versionWriter classifies the errors of a container's version writer.
type versionWriter struct {
	storage.VersionWriter
	c *container
}

func (w *versionWriter) Write(p []byte) (int, error) {
	n, err := w.VersionWriter.Write(p)
	return n, w.c.classify(err)
}

func (w *versionWriter) Commit(ctx context.Context, info storage.ObjectInfo) (storage.ObjectInfo, error) {
	out, err := w.VersionWriter.Commit(ctx, info)
	return out, w.c.classify(err)
}

// partWriter classifies the errors of a container's part writer.
type partWriter struct {
	storage.PartWriter
	c *container
}

func (w *partWriter) Write(p []byte) (int, error) {
	n, err := w.PartWriter.Write(p)
	return n, w.c.classify(err)
}

func (w *partWriter) Commit(ctx context.Context, meta storage.PartMeta) (storage.PartInfo, error) {
	info, err := w.PartWriter.Commit(ctx, meta)
	return info, w.c.classify(err)
}

// objectReader classifies the errors of a container's object reader.
type objectReader struct {
	io.ReadSeekCloser
	c *container
}

func (r *objectReader) Read(p []byte) (int, error) {
	n, err := r.ReadSeekCloser.Read(p)
	if err == io.EOF {
		return n, err
	}
	return n, r.c.classify(err)
}

// preparingReader is an objectReader over a reader that implements
// storage.PreparingReader (a restic restore), which it keeps visible.
type preparingReader struct {
	objectReader
	p storage.PreparingReader
}

func (r *preparingReader) Prepare() error { return r.c.classify(r.p.Prepare()) }

type rangePreparingReader struct {
	preparingReader
	rp storage.RangePreparingReader
}

func (r *rangePreparingReader) PrepareRange(start, length int64) error {
	return r.c.classify(r.rp.PrepareRange(start, length))
}

// wrapObjectWriter, wrapVersionWriter, wrapPartWriter and wrapReader
// return w (r) unchanged for the home. The wrappers keep the optional
// interfaces callers look for: storage.PreparingReader on readers (the
// writers' callers look for none).
func (c *container) wrapObjectWriter(w storage.ObjectWriter) storage.ObjectWriter {
	if c.home || w == nil {
		return w
	}
	return &objectWriter{ObjectWriter: w, c: c}
}

func (c *container) wrapVersionWriter(w storage.VersionWriter) storage.VersionWriter {
	if c.home || w == nil {
		return w
	}
	return &versionWriter{VersionWriter: w, c: c}
}

func (c *container) wrapPartWriter(w storage.PartWriter) storage.PartWriter {
	if c.home || w == nil {
		return w
	}
	return &partWriter{PartWriter: w, c: c}
}

func (c *container) wrapReader(r io.ReadSeekCloser) io.ReadSeekCloser {
	if c.home || r == nil {
		return r
	}
	if p, ok := r.(storage.PreparingReader); ok {
		if rp, ok := r.(storage.RangePreparingReader); ok {
			return &rangePreparingReader{preparingReader: preparingReader{objectReader: objectReader{ReadSeekCloser: r, c: c}, p: p}, rp: rp}
		}
		return &preparingReader{objectReader: objectReader{ReadSeekCloser: r, c: c}, p: p}
	}
	return &objectReader{ReadSeekCloser: r, c: c}
}
