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

	mu    sync.Mutex
	off   int64 // Read/Seek offset
	spool *spool
	file  *os.File
	data  *crypt.Reader
	// A nonzero rangeSize means PrepareRange selected a partial spool.
	rangeStart, rangeSize int64
	err                   error // sticky preparation error
	closed                bool
}

var (
	_ storage.PreparingReader      = (*Reader)(nil)
	_ storage.RangePreparingReader = (*Reader)(nil)
	_ io.ReadSeekCloser            = (*Reader)(nil)
	_ io.ReaderAt                  = (*Reader)(nil)
)

// errReaderClosed is returned by a closed Reader.
var errReaderClosed = errors.New("restic: read from a closed object reader")

// DefaultRestoreSpoolBytes bounds disk reservations across concurrent
// restores in this process. A full restore over this size is refused before
// restic starts; ranged reads reserve only their requested bytes.
const DefaultRestoreSpoolBytes int64 = 1 << 30

var ErrRestoreSpoolLimit = fmt.Errorf("restic: restore spool limit exceeded: %w", storage.ErrResourceLimit)

var restoreSpoolBudget struct {
	sync.Mutex
	bytes int64
}

// Prepare restores the version now (see storage.PreparingReader).
func (r *Reader) Prepare() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prepare()
}

// PrepareRange restores only [start, start+length). The reader is then
// restricted to that range. A caller that needs arbitrary access uses
// Prepare instead.
func (r *Reader) PrepareRange(start, length int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if start < 0 || length <= 0 || start > r.size || length > r.size-start {
		return errors.New("restic: invalid read range")
	}
	if r.data != nil {
		if r.rangeSize == 0 || r.rangeStart == start && r.rangeSize == length {
			return nil
		}
		return errors.New("restic: reader prepared for a different range")
	}
	if r.err != nil {
		return r.err
	}
	r.rangeStart, r.rangeSize = start, length
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
	start, length := int64(0), r.size
	if r.rangeSize != 0 {
		start, length = r.rangeStart, r.rangeSize
	}
	sp, err := r.rt.acquireSpool(r.ctx, r.loc, r.snapshot, r.size, start, length)
	if err != nil {
		r.err = err
		return err
	}
	f, err := os.Open(sp.path)
	if err == nil {
		if r.data, err = crypt.NewReader(f, sp.dataKey, length); err != nil {
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
	start, end := r.readBounds()
	if r.off < start {
		return 0, errors.New("restic: read before prepared range")
	}
	if r.off >= end {
		return 0, io.EOF
	}
	if err := r.prepare(); err != nil {
		return 0, err
	}
	if rest := end - r.off; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := r.data.ReadAt(p, r.off-start)
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
	start, end := r.readBounds()
	if off < start {
		return 0, errors.New("restic: read outside prepared range")
	}
	if off >= end {
		return 0, io.EOF
	}
	short := r.rangeSize != 0 && int64(len(p)) > end-off
	if short {
		p = p[:end-off]
	}
	if err := r.prepare(); err != nil {
		return 0, err
	}
	n, err := r.data.ReadAt(p, off-start)
	if short && err == nil {
		err = io.EOF
	}
	return n, err
}

// readBounds returns the logical offsets available from this reader.
// r.mu is held.
func (r *Reader) readBounds() (int64, int64) {
	if r.rangeSize == 0 {
		return 0, r.size
	}
	return r.rangeStart, r.rangeStart + r.rangeSize
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
	id        string // Runtime.spools key
	size      int64  // encrypted file size reserved against the disk quota
	plainSize int64
	done      chan struct{}
	cancel    context.CancelFunc

	// Guarded by Runtime.mu.
	refs     int
	finished bool
	charged  bool // size is included in Runtime.spoolBytes

	// Set before done is closed.
	path    string
	dataKey []byte // the spool file's encryption key
	err     error
}

// acquireSpool returns the restored snapshot of the repository loc,
// starting its restore when no reader has one, and waits until it is
// complete or ctx ends. The caller must releaseSpool it.
func (rt *Runtime) acquireSpool(ctx context.Context, loc Location, snapshot string, size, start, length int64) (*spool, error) {
	charge := crypt.EncryptedSize(length)
	if charge < 0 {
		return nil, ErrRestoreSpoolLimit
	}
	k := loc.Repository + "\x00" + snapshot
	if start != 0 || length != size {
		k += fmt.Sprintf("\x00%d\x00%d", start, length)
	}
	rt.mu.Lock()
	if rt.spools == nil {
		rt.spools = make(map[string]*spool)
	}
	sp := rt.spools[k]
	if sp == nil {
		limit := rt.MaxRestoreSpoolBytes
		if limit <= 0 {
			limit = DefaultRestoreSpoolBytes
		}
		restoreSpoolBudget.Lock()
		if charge > limit-rt.spoolBytes || charge > limit-restoreSpoolBudget.bytes {
			reserved := restoreSpoolBudget.bytes
			restoreSpoolBudget.Unlock()
			rt.mu.Unlock()
			return nil, fmt.Errorf("%w: %d-byte encrypted restore, %d bytes already reserved across backings, %d-byte limit", ErrRestoreSpoolLimit, charge, reserved, limit)
		}
		restoreSpoolBudget.bytes += charge
		restoreSpoolBudget.Unlock()
		dumpCtx, cancel := context.WithCancel(context.Background())
		sp = &spool{id: k, size: charge, plainSize: length, charged: true, done: make(chan struct{}), cancel: cancel}
		rt.spoolBytes += charge
		rt.spools[k] = sp
		rt.restores.Go(func() { rt.runSpool(dumpCtx, sp, loc, snapshot, size, start, length) })
	} else if sp.plainSize != length {
		rt.mu.Unlock()
		return nil, errors.New("restic: snapshot size differs between readers")
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
func (rt *Runtime) runSpool(ctx context.Context, sp *spool, loc Location, snapshot string, size, start, length int64) {
	path, key, err := rt.dump(ctx, loc, snapshot, size, start, length)
	rt.mu.Lock()
	sp.path, sp.dataKey, sp.err, sp.finished = path, key, err, true
	orphan := err == nil && sp.refs == 0
	if err != nil && rt.spools[sp.id] == sp {
		delete(rt.spools, sp.id) // let the next reader try again
	}
	if err != nil && sp.charged {
		rt.releaseChargeLocked(sp)
	}
	rt.mu.Unlock()
	if orphan {
		if removeErr := os.Remove(path); removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			rt.mu.Lock()
			rt.releaseChargeLocked(sp)
			rt.mu.Unlock()
		} else if rt.Log != nil {
			rt.Log.Warn("restic: could not remove orphaned restore spool", "error", removeErr)
		}
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
		if removeErr := os.Remove(remove); removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			rt.mu.Lock()
			rt.releaseChargeLocked(sp)
			rt.mu.Unlock()
		} else if rt.Log != nil {
			rt.Log.Warn("restic: could not remove restore spool", "error", removeErr)
		}
	}
}

// releaseChargeLocked releases disk reserved for sp after its file is gone
// (or after a failed dump removed it). rt.mu is held.
func (rt *Runtime) releaseChargeLocked(sp *spool) {
	if !sp.charged {
		return
	}
	restoreSpoolBudget.Lock()
	restoreSpoolBudget.bytes -= sp.size
	restoreSpoolBudget.Unlock()
	rt.spoolBytes -= sp.size
	sp.charged = false
}

// dump restores the snapshot of a version of size bytes into an encrypted
// temporary file and returns its path and key. The repository is read
// without a lock (dump only reads; snapshots referenced by gos3 are never
// forgotten).
func (rt *Runtime) dump(ctx context.Context, loc Location, snapshot string, size, start, length int64) (string, []byte, error) {
	dumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := rt.Command(dumpCtx, loc, "--no-lock", "dump", snapshot, "/object")
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
	full := start == 0 && length == size
	var out io.Writer
	var whole *boundedWriter
	var part *rangeWriter
	if full {
		whole = &boundedWriter{dst: ew, remaining: size}
		out = whole
	} else {
		part = &rangeWriter{dst: ew, start: start, length: length, done: cancel}
		out = part
	}
	var diag stderrTail
	cmd.Stdout, cmd.Stderr = out, &diag
	runErr := cmd.Run()
	if full && runErr != nil || !full && !part.completed {
		if runErr == nil {
			runErr = fmt.Errorf("restored %d bytes of the requested %d-byte range", part.copied, length)
		}
		return fail(rt.failure(ctx, "retrieve object", runErr, &diag, loc))
	}
	if full && whole.remaining != 0 {
		got := size - whole.remaining
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

// rangeWriter stores only the requested bytes. As soon as the range is
// complete it cancels restic: the remaining object need not be restored.
type rangeWriter struct {
	dst           io.Writer
	start, length int64
	seen, copied  int64
	completed     bool
	done          context.CancelFunc
}

func (w *rangeWriter) Write(p []byte) (int, error) {
	chunkStart := w.seen
	w.seen += int64(len(p))
	from := max(w.start-chunkStart, 0)
	to := min(w.start+w.length-chunkStart, int64(len(p)))
	if from < to {
		n, err := w.dst.Write(p[from:to])
		w.copied += int64(n)
		if err != nil {
			return n, err
		}
		if n != int(to-from) {
			return n, io.ErrShortWrite
		}
	}
	if w.copied == w.length {
		w.completed = true
		w.done()
		return len(p), io.EOF
	}
	return len(p), nil
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
