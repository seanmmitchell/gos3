package restic

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
)

// Open returns a reader of the snapshot (see ValidSnapshot) of a version of
// size bytes in the repository loc. Nothing is restored until the reader is
// read or prepared (storage.PreparingReader); a reader of an empty version
// never runs restic.
func (rt *Runtime) Open(ctx context.Context, loc Location, snapshot string, size int64) (*Reader, error) {
	if !ValidSnapshot(snapshot) {
		return nil, fmt.Errorf("restic: invalid snapshot ID %q", snapshot)
	}
	if size < 0 {
		return nil, fmt.Errorf("restic: invalid object size %d", size)
	}
	return &Reader{rt: rt, ctx: ctx, loc: loc, snapshot: snapshot, size: size}, nil
}

// Reader reads a restic version. On first use it restores the version into
// a spool shared with concurrent readers of the same snapshot (see spool)
// and then reads that. It implements io.ReadSeekCloser, io.ReaderAt and
// storage.PreparingReader, and is safe for concurrent use.
type Reader struct {
	rt       *Runtime
	ctx      context.Context
	loc      Location
	snapshot string
	size     int64

	mu     sync.Mutex
	off    int64 // Read/Seek offset
	spool  *spool
	file   *os.File
	data   *crypt.Reader
	err    error // sticky preparation error
	closed bool
}

var (
	_ storage.PreparingReader = (*Reader)(nil)
	_ io.ReadSeekCloser       = (*Reader)(nil)
	_ io.ReaderAt             = (*Reader)(nil)
)

// errReaderClosed is returned by a closed Reader.
var errReaderClosed = errors.New("restic: read from a closed object reader")

// Prepare restores the version now (see storage.PreparingReader).
func (r *Reader) Prepare() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prepare()
}

// prepare implements Prepare; r.mu is held.
func (r *Reader) prepare() error {
	switch {
	case r.closed:
		return errReaderClosed
	case r.data != nil || r.size == 0:
		return nil
	case r.err != nil:
		return r.err
	}
	sp, err := r.rt.acquireSpool(r.ctx, r.loc, r.snapshot, r.size)
	if err != nil {
		r.err = err
		return err
	}
	f, err := os.Open(sp.path)
	if err == nil {
		if r.data, err = crypt.NewReader(f, sp.dataKey, r.size); err != nil {
			_ = f.Close()
		}
	}
	if err != nil {
		r.rt.releaseSpool(sp)
		r.err = fmt.Errorf("restic: open restored object: %w", err)
		return r.err
	}
	r.spool, r.file = sp, f
	return nil
}

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errReaderClosed
	}
	if r.off >= r.size {
		return 0, io.EOF
	}
	if err := r.prepare(); err != nil {
		return 0, err
	}
	if rest := r.size - r.off; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := r.data.ReadAt(p, r.off)
	r.off += int64(n)
	if err == io.EOF && n == len(p) {
		err = nil
	}
	return n, err
}

// ReadAt implements io.ReaderAt.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errReaderClosed
	}
	if off < 0 {
		return 0, errors.New("restic: negative read offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	if err := r.prepare(); err != nil {
		return 0, err
	}
	return r.data.ReadAt(p, off)
}

// Seek implements io.Seeker; it restores nothing.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.off
	case io.SeekEnd:
		offset += r.size
	default:
		return 0, errors.New("restic: invalid seek whence")
	}
	if offset < 0 {
		return 0, errors.New("restic: negative seek position")
	}
	r.off = offset
	return offset, nil
}

// Close releases the restored data; subsequent calls do nothing.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.rt.releaseSpool(r.spool)
	r.file, r.data, r.spool = nil, nil, nil
	return err
}

// spool is one restored restic version: `restic dump` output in an
// encrypted temporary file (bounded memory, random access, no plaintext on
// disk). Concurrent readers of the same snapshot share it, so parallel
// ranged downloads restore an object once. The restore runs on its own
// context, canceled when every reader waiting for it has gone; the file is
// removed when its last reader closes.
type spool struct {
	id     string // Runtime.spools key
	done   chan struct{}
	cancel context.CancelFunc

	// Guarded by Runtime.mu.
	refs     int
	finished bool

	// Set before done is closed.
	path    string
	dataKey []byte // the spool file's encryption key
	err     error
}

// acquireSpool returns the restored snapshot of the repository loc,
// starting its restore when no reader has one, and waits until it is
// complete or ctx ends. The caller must releaseSpool it.
func (rt *Runtime) acquireSpool(ctx context.Context, loc Location, snapshot string, size int64) (*spool, error) {
	k := loc.Repository + "\x00" + snapshot
	rt.mu.Lock()
	if rt.spools == nil {
		rt.spools = make(map[string]*spool)
	}
	sp := rt.spools[k]
	if sp == nil {
		dumpCtx, cancel := context.WithCancel(context.Background())
		sp = &spool{id: k, done: make(chan struct{}), cancel: cancel}
		rt.spools[k] = sp
		go rt.runSpool(dumpCtx, sp, loc, snapshot, size)
	}
	sp.refs++
	rt.mu.Unlock()
	select {
	case <-sp.done:
	case <-ctx.Done():
		rt.releaseSpool(sp)
		return nil, fmt.Errorf("restic: retrieve object: %w", ctx.Err())
	}
	if sp.err != nil {
		rt.releaseSpool(sp)
		return nil, sp.err
	}
	return sp, nil
}

// runSpool restores the spool sp.
func (rt *Runtime) runSpool(ctx context.Context, sp *spool, loc Location, snapshot string, size int64) {
	path, key, err := rt.dump(ctx, loc, snapshot, size)
	rt.mu.Lock()
	sp.path, sp.dataKey, sp.err, sp.finished = path, key, err, true
	orphan := err == nil && sp.refs == 0
	if err != nil && rt.spools[sp.id] == sp {
		delete(rt.spools, sp.id) // let the next reader try again
	}
	rt.mu.Unlock()
	if orphan {
		_ = os.Remove(path)
	}
	sp.cancel()
	close(sp.done)
}

// releaseSpool drops a reference to sp, canceling its restore or removing
// its file with the last one.
func (rt *Runtime) releaseSpool(sp *spool) {
	var remove string
	rt.mu.Lock()
	sp.refs--
	if sp.refs == 0 {
		if rt.spools[sp.id] == sp {
			delete(rt.spools, sp.id)
		}
		switch {
		case !sp.finished:
			sp.cancel()
		case sp.err == nil:
			remove = sp.path
		}
	}
	rt.mu.Unlock()
	if remove != "" {
		_ = os.Remove(remove)
	}
}

// dump restores the snapshot of a version of size bytes into an encrypted
// temporary file and returns its path and key. The repository is read
// without a lock (dump only reads; snapshots referenced by gos3 are never
// forgotten).
func (rt *Runtime) dump(ctx context.Context, loc Location, snapshot string, size int64) (string, []byte, error) {
	cmd, err := rt.Command(ctx, loc, "--no-lock", "dump", snapshot, "/object")
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(rt.TempDir, "restic-*")
	if err != nil {
		return "", nil, fmt.Errorf("restic: create temporary file: %w", err)
	}
	fail := func(err error) (string, []byte, error) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	// A fresh key: the spool is the only stream it encrypts.
	key := make([]byte, crypt.KeySize)
	if _, err := rand.Read(key); err != nil {
		return fail(fmt.Errorf("restic: spool key: %w", err))
	}
	bw := bufio.NewWriterSize(f, 256<<10)
	ew, err := crypt.NewWriter(bw, key)
	if err != nil {
		return fail(err)
	}
	out := &boundedWriter{dst: ew, remaining: size}
	var diag stderrTail
	cmd.Stdout, cmd.Stderr = out, &diag
	if err := cmd.Run(); err != nil {
		return fail(rt.failure(ctx, "retrieve object", err, &diag, loc))
	}
	if got := size - out.remaining; got != size {
		return fail(rt.failure(ctx, "retrieve object", fmt.Errorf("restored %d bytes, expected %d", got, size), nil, loc))
	}
	if err := ew.Close(); err != nil {
		return fail(fmt.Errorf("restic: write restored object: %w", err))
	}
	if err := bw.Flush(); err != nil {
		return fail(fmt.Errorf("restic: write restored object: %w", err))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("restic: write restored object: %w", err)
	}
	return f.Name(), key, nil
}

// boundedWriter writes at most remaining bytes to dst.
type boundedWriter struct {
	dst       io.Writer
	remaining int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("restic: restored object exceeds expected size")
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	return n, err
}
