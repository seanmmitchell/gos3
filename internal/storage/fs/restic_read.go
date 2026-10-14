package fs

import (
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

// openRestic returns the reader of a restic version of size bytes in the
// bucket with ID id whose snapshot reference is f (closed here). Nothing
// is restored until the reader is read or prepared.
func (b *Backend) openRestic(ctx context.Context, f *os.File, id string, size int64) (io.ReadSeekCloser, error) {
	defer f.Close()
	ref, err := io.ReadAll(io.LimitReader(f, snapshotRefSize+1))
	if err != nil {
		return nil, err
	}
	if !validSnapshot(string(ref)) {
		return nil, fmt.Errorf("%w: invalid restic snapshot reference", errCorrupt)
	}
	return &resticReader{b: b, ctx: ctx, id: id, snapshot: string(ref), size: size}, nil
}

// resticReader reads a restic version. On first use it restores the
// version into a spool shared with concurrent readers of the same snapshot
// (see resticSpool) and then reads that.
type resticReader struct {
	b        *Backend
	ctx      context.Context
	id       string
	snapshot string
	size     int64

	mu     sync.Mutex
	off    int64 // Read/Seek offset
	spool  *resticSpool
	data   readerAtCloser
	err    error // sticky preparation error
	closed bool
}

var _ storage.PreparingReader = (*resticReader)(nil)

// readerAtCloser is the decrypting reader over a spool file.
type readerAtCloser interface {
	io.ReaderAt
	io.Closer
}

// errReaderClosed is returned by a closed resticReader.
var errReaderClosed = errors.New("fs: read from a closed object reader")

// Prepare restores the version now (see storage.PreparingReader).
func (r *resticReader) Prepare() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prepare()
}

// prepare implements Prepare; r.mu is held.
func (r *resticReader) prepare() error {
	switch {
	case r.closed:
		return errReaderClosed
	case r.data != nil || r.size == 0:
		return nil
	case r.err != nil:
		return r.err
	}
	sp, err := r.b.acquireSpool(r.ctx, r.id, r.snapshot, r.size)
	if err != nil {
		r.err = err
		return err
	}
	f, err := os.Open(sp.path)
	if err == nil {
		r.data, err = decryptFile(f, sp.dataKey, r.size, f.Close)
		if err != nil {
			_ = f.Close()
		}
	}
	if err != nil {
		r.b.releaseSpool(sp)
		r.err = fmt.Errorf("fs: open restored object: %w", err)
		return r.err
	}
	r.spool = sp
	return nil
}

func (r *resticReader) Read(p []byte) (int, error) {
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

func (r *resticReader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errReaderClosed
	}
	if off < 0 {
		return 0, errors.New("fs: negative read offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	if err := r.prepare(); err != nil {
		return 0, err
	}
	return r.data.ReadAt(p, off)
}

func (r *resticReader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.off
	case io.SeekEnd:
		offset += r.size
	default:
		return 0, errors.New("fs: invalid seek whence")
	}
	if offset < 0 {
		return 0, errors.New("fs: negative seek position")
	}
	r.off = offset
	return offset, nil
}

// Close releases the restored data; subsequent calls do nothing.
func (r *resticReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.data == nil {
		return nil
	}
	err := r.data.Close()
	r.b.releaseSpool(r.spool)
	return err
}

// resticSpool is one restored restic version: `restic dump` output in an
// encrypted temporary file (bounded memory, random access, no plaintext on
// disk). Concurrent readers of the same snapshot share it, so parallel
// ranged downloads restore an object once. The restore runs on its own
// context, canceled when every reader waiting for it has gone; the file is
// removed when its last reader closes.
type resticSpool struct {
	key    string // resticState.spools key
	done   chan struct{}
	cancel context.CancelFunc

	// Guarded by resticState.mu.
	refs     int
	finished bool

	// Set before done is closed.
	path    string
	dataKey []byte
	err     error
}

// acquireSpool returns the restored snapshot of the bucket with ID id,
// starting its restore when no reader has one, and waits until it is
// complete or ctx ends. The caller must releaseSpool it.
func (b *Backend) acquireSpool(ctx context.Context, id, snapshot string, size int64) (*resticSpool, error) {
	st := &b.restic
	k := id + "/" + snapshot
	st.mu.Lock()
	if st.spools == nil {
		st.spools = make(map[string]*resticSpool)
	}
	sp := st.spools[k]
	if sp == nil {
		dumpCtx, cancel := context.WithCancel(context.Background())
		sp = &resticSpool{key: k, done: make(chan struct{}), cancel: cancel}
		st.spools[k] = sp
		go b.runSpool(dumpCtx, sp, id, snapshot, size)
	}
	sp.refs++
	st.mu.Unlock()
	select {
	case <-sp.done:
	case <-ctx.Done():
		b.releaseSpool(sp)
		return nil, fmt.Errorf("restic: retrieve object: %w", ctx.Err())
	}
	if sp.err != nil {
		b.releaseSpool(sp)
		return nil, sp.err
	}
	return sp, nil
}

// runSpool restores the spool sp.
func (b *Backend) runSpool(ctx context.Context, sp *resticSpool, id, snapshot string, size int64) {
	path, key, err := b.dumpSnapshot(ctx, id, snapshot, size)
	st := &b.restic
	st.mu.Lock()
	sp.path, sp.dataKey, sp.err, sp.finished = path, key, err, true
	orphan := err == nil && sp.refs == 0
	if err != nil && st.spools[sp.key] == sp {
		delete(st.spools, sp.key) // let the next reader try again
	}
	st.mu.Unlock()
	if orphan {
		_ = os.Remove(path)
	}
	sp.cancel()
	close(sp.done)
}

// releaseSpool drops a reference to sp, canceling its restore or removing
// its file with the last one.
func (b *Backend) releaseSpool(sp *resticSpool) {
	st := &b.restic
	var remove string
	st.mu.Lock()
	sp.refs--
	if sp.refs == 0 {
		if st.spools[sp.key] == sp {
			delete(st.spools, sp.key)
		}
		switch {
		case !sp.finished:
			sp.cancel()
		case sp.err == nil:
			remove = sp.path
		}
	}
	st.mu.Unlock()
	if remove != "" {
		_ = os.Remove(remove)
	}
}

// dumpSnapshot restores the snapshot of a version of size bytes into an
// encrypted temporary file and returns its path and key. The repository is
// read without a lock (dump only reads; snapshots referenced by gos3 are
// never forgotten).
func (b *Backend) dumpSnapshot(ctx context.Context, id, snapshot string, size int64) (string, []byte, error) {
	cfg, err := b.resticSettings(id)
	if err != nil {
		return "", nil, err
	}
	cmd, err := resticCommandFor(ctx, cfg, "--no-lock", "dump", snapshot, "/object")
	if err != nil {
		return "", nil, err
	}
	t := b.newTemp()
	// A fresh key: the spool is the only stream it encrypts.
	t.key = make([]byte, crypt.KeySize)
	_, _ = rand.Read(t.key)
	var diag stderrTail
	cmd.Stdout, cmd.Stderr = &boundedObjectWriter{dst: t, remaining: size}, &diag
	if err := cmd.Run(); err != nil {
		t.discard()
		return "", nil, b.resticFailure(ctx, "retrieve object", err, &diag, cfg)
	}
	if t.size != size {
		t.discard()
		return "", nil, b.resticFailure(ctx, "retrieve object",
			fmt.Errorf("restored %d bytes, expected %d", t.size, size), nil, cfg)
	}
	if err := t.finish(false, true); err != nil {
		return "", nil, err
	}
	return t.path, t.key, nil
}

// boundedObjectWriter writes at most remaining bytes to dst.
type boundedObjectWriter struct {
	dst       io.Writer
	remaining int64
}

func (w *boundedObjectWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("restic: restored object exceeds expected size")
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	return n, err
}
