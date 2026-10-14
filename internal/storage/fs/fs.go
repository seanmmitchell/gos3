// Package fs implements a durable, filesystem-backed storage.Backend.
//
// # Layout
//
// Everything lives below <root>/.gos3/:
//
//	meta.db                         bbolt metadata index (buckets, configs,
//	                                object versions, multipart uploads)
//	data/<bucket-id>/<2-hex>/<id>   immutable object data files
//	tmp/                            files being written
//	uploads/<upload-id>/<n>-<id>    multipart part data (n = part number)
//	orphans/<run>/...               files the recovery pass found
//	                                unreferenced, kept for a while (see
//	                                Recovery)
//
// The schema of meta.db is documented in schema.go.
//
// # Write path
//
// Object data is streamed into a fresh file in tmp/ (optionally fsync'd,
// see Options.FsyncData), renamed to its final, never reused name under
// data/ and only then is the metadata committed in a single bbolt write
// transaction, which also evaluates conditional-write preconditions. The
// bbolt write transaction is never held while streaming data. Data files of
// versions replaced or deleted by a transaction are unlinked after it
// commits; when a commit fails (precondition failed, bucket deleted, ...)
// the new data file is removed. Empty objects have no data file.
//
// Multipart parts are written the same way into uploads/<upload-id>/ with a
// unique file name per part upload, so re-uploading a part number never
// races with readers of the previous part. CompleteMultipartUpload streams
// the listed parts into a new data file and then commits like a PutObject.
//
// # Readers
//
// OpenObject returns an *os.File. Because data files are immutable and
// unlinked (never rewritten) when superseded, an open reader keeps returning
// the content of the version it opened even if the object is concurrently
// overwritten or deleted (POSIX unlink semantics). On Windows an open file
// cannot be deleted: unlinking a superseded data file that is still being
// read fails, the failure is logged and the orphan is removed by the
// recovery pass at the next start.
//
// # Recovery
//
// Open empties tmp/ synchronously and then starts a background pass that
// collects data files and upload part files no longer referenced by the
// metadata (left behind by crashes between the rename and the metadata
// commit, or between the commit and the unlink), as well as directories of
// deleted buckets and finished uploads. Files of writes in progress are
// tracked in memory and never touched by the pass.
//
// The pass never deletes what it finds: it moves it to
// orphans/<start time>-<random>/ (mirroring its path below .gos3) and
// deletes such quarantine directories only once they are older than
// Options.OrphanRetention. Data written after a backup of meta.db therefore
// survives restoring that backup for a while and can be recovered by hand.
// Entries whose names gos3 never generates (e.g. lost+found when data/ is a
// mount point) are left alone. Open refuses to start when meta.db is missing
// or empty while data/ or uploads/ hold files, since a fresh database would
// make every stored object look orphaned.
//
// # Durability
//
// Metadata transactions are always fsync'd by bbolt. With FsyncData the data
// file and its directory are fsync'd before the metadata commit, so a
// committed object survives power loss; without it a crash may leave
// recently written objects truncated (OpenObject then reports an error for
// them instead of returning short content).
//
// Only one process may open a root at a time: Open fails after one second
// if another process holds the metadata database.
package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
)

// ErrClosed is returned by every operation after Close.
var ErrClosed = errors.New("fs: backend closed")

// Options configures Open.
type Options struct {
	ResticS3 *storage.ResticS3
	// FsyncData fsyncs every data file (and its directory) before the
	// metadata referencing it is committed. Slower, but committed objects
	// survive power loss.
	FsyncData bool
	// Logger receives recovery statistics and non-fatal errors (e.g. failed
	// unlinks). nil discards logs.
	Logger *slog.Logger
	// OrphanRetention is how long the recovery pass keeps unreferenced
	// files it moved to .gos3/orphans/ before deleting them (at a later
	// start). Zero or negative means DefaultOrphanRetention.
	OrphanRetention time.Duration
}

// DefaultOrphanRetention is the default of Options.OrphanRetention.
const DefaultOrphanRetention = 7 * 24 * time.Hour

// lockTimeout bounds how long Open waits for the metadata database lock.
const lockTimeout = time.Second

const (
	dirPerm  = 0o700 // the system directory holds credentials and metadata
	filePerm = 0o600
)

// Backend is a filesystem storage.Backend. Create it with Open.
type Backend struct {
	// keyMu guards keys, the cache of bucket master keys by bucket ID
	// (see bucketKey).
	keyMu          sync.RWMutex
	keys           map[string]*bucketKeyEntry
	resticMu       sync.Mutex
	resticConfigMu sync.RWMutex
	resticS3       *storage.ResticS3
	root           string
	sysDir         string
	dataDir        string
	tmpDir         string
	uploadDir      string
	orphanDir      string
	retention      time.Duration
	fsync          bool
	log            *slog.Logger
	db             *bbolt.DB

	closed atomic.Bool
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// recoveryDone is closed when the startup recovery pass has finished.
	recoveryDone chan struct{}

	pending pendingSet
	commits committer

	// commitFault, when set (tests only), replaces the result of every
	// successful write transaction, simulating a commit that reports an
	// error although it reached the disk (e.g. a failed fsync).
	commitFault func() error
}

var (
	_ storage.Backend            = (*Backend)(nil)
	_ storage.ConditionalDeleter = (*Backend)(nil)
)
var (
	_ storage.ConditionalDeleter = (*Backend)(nil)
	_ storage.UploadWalker       = (*Backend)(nil)
)

// Open opens (creating if needed) a filesystem backend rooted at root.
func Open(root string, opts Options) (*Backend, error) {
	if root == "" {
		return nil, errors.New("fs: empty root directory")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fs: resolve root: %w", err)
	}
	b := &Backend{
		root:         abs,
		sysDir:       filepath.Join(abs, ".gos3"),
		fsync:        opts.FsyncData,
		log:          opts.Logger,
		recoveryDone: make(chan struct{}),
		pending:      pendingSet{m: make(map[string]int)},
	}
	if b.log == nil {
		b.log = slog.New(slog.DiscardHandler)
	}
	b.dataDir = filepath.Join(b.sysDir, "data")
	b.tmpDir = filepath.Join(b.sysDir, "tmp")
	b.uploadDir = filepath.Join(b.sysDir, "uploads")
	b.orphanDir = filepath.Join(b.sysDir, "orphans")
	b.retention = opts.OrphanRetention
	if b.retention <= 0 {
		b.retention = DefaultOrphanRetention
	}
	for _, d := range []string{b.sysDir, b.dataDir, b.tmpDir, b.uploadDir} {
		if err := os.MkdirAll(d, dirPerm); err != nil {
			return nil, fmt.Errorf("fs: create %s: %w", d, err)
		}
		b.tightenPerm(d)
	}
	b.tightenPerm(b.orphanDir)
	b.protectKeys()

	dbPath := filepath.Join(b.sysDir, "meta.db")
	if err := b.checkMetadataPresent(dbPath); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(dbPath, filePerm, &bbolt.Options{
		Timeout:        lockTimeout,
		FreelistType:   bbolt.FreelistMapType,
		NoFreelistSync: true,
	})
	if err != nil {
		if errors.Is(err, berrors.ErrTimeout) {
			return nil, fmt.Errorf("fs: metadata database %s is locked by another process (is another gos3 using this data directory?)", dbPath)
		}
		return nil, fmt.Errorf("fs: open metadata database %s: %w", dbPath, err)
	}
	b.db = db
	if err := b.loadResticBacking(opts.ResticS3); err != nil {
		_ = db.Close()
		return nil, err
	}
	b.tightenPerm(dbPath)
	if err := b.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// The database lock is held, so no other process uses tmp/.
	if n, err := clearDir(b.tmpDir); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("fs: clean %s: %w", b.tmpDir, err)
	} else if n > 0 {
		b.log.Info("fs: removed temporary files left by a previous run", "count", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(b.recoveryDone)
		b.recover(ctx)
	}()
	return b, nil
}

// tightenPerm removes the group and other permission bits of path when it
// exists with them set. MkdirAll and bbolt.Open only apply dirPerm/filePerm
// when they create a path; a .gos3 directory or meta.db pre-created by an
// operator or a container volume, or restored from a backup that did not
// keep modes, would otherwise stay readable by other local users (meta.db
// holds every key, metadata entry and bucket policy). A failure (e.g. a
// mount point owned by another user) is logged, not fatal. Windows has no
// mode bits to fix.
func (b *Backend) tightenPerm(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	perm := fi.Mode().Perm()
	if perm&0o077 == 0 {
		return
	}
	if err := os.Chmod(path, perm&^0o077); err != nil {
		b.log.Warn("fs: cannot restrict permissions; other local users may read it", "path", path, "mode", perm.String(), "err", err)
		return
	}
	b.log.Warn("fs: restricted permissions that allowed access by other local users", "path", path, "was", perm.String(), "now", (perm &^ 0o077).String())
}

// checkMetadataPresent refuses to proceed when the metadata database is
// missing or empty (bbolt would silently create a fresh one) although the
// data or uploads directory holds files gos3 wrote: the recovery pass would
// then consider every object orphaned. The check runs before the database
// lock is taken; a database that is missing or empty cannot be in use.
func (b *Backend) checkMetadataPresent(dbPath string) error {
	fi, err := os.Stat(dbPath)
	switch {
	case err == nil && fi.Size() > 0:
		return nil
	case err != nil && !errors.Is(err, iofs.ErrNotExist):
		return nil // let bbolt.Open report it
	}
	dir, err := b.findStoredFile()
	if err != nil {
		return fmt.Errorf("fs: inspect %s: %w", b.sysDir, err)
	}
	if dir == "" {
		return nil
	}
	state := "missing"
	if fi != nil {
		state = "empty"
	}
	return fmt.Errorf("fs: metadata database %s is %s but %s still holds stored data; "+
		"refusing to start with an empty index, which would discard it. Restore meta.db "+
		"from a backup, or move %s and %s aside to start with an empty store",
		dbPath, state, dir, b.dataDir, b.uploadDir)
}

// findStoredFile returns the directory (data or uploads) holding at least one
// file below a bucket or upload directory gos3 could have created, or "".
func (b *Backend) findStoredFile() (string, error) {
	errFound := errors.New("found")
	for _, area := range []struct {
		dir   string
		valid func(string) bool
	}{{b.dataDir, validBucketID}, {b.uploadDir, validUploadID}} {
		entries, err := os.ReadDir(area.dir)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if !e.IsDir() || !area.valid(e.Name()) {
				continue
			}
			err := filepath.WalkDir(filepath.Join(area.dir, e.Name()), func(_ string, d iofs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					return errFound
				}
				return nil
			})
			if errors.Is(err, errFound) {
				return area.dir, nil
			}
			if err != nil {
				return "", err
			}
		}
	}
	return "", nil
}

func (b *Backend) initSchema() error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{topMeta, topBuckets, topUploads} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("fs: init schema: %w", err)
			}
		}
		m := tx.Bucket(topMeta)
		switch v := m.Get(keyFormat); {
		case v == nil:
			return m.Put(keyFormat, []byte{schemaVersion})
		case len(v) != 1 || v[0] != schemaVersion:
			return fmt.Errorf("fs: unsupported metadata schema version %v", v)
		}
		if err := ensureUploadIndexes(tx); err != nil {
			return fmt.Errorf("fs: index multipart uploads: %w", err)
		}
		return nil
	})
}

// Root returns the absolute root directory of the backend.
func (b *Backend) Root() string { return b.root }

// Close stops background work and closes the metadata database. Subsequent
// operations return ErrClosed; readers returned by OpenObject keep working.
// Close is idempotent.
func (b *Backend) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	b.cancel()
	b.wg.Wait()
	if err := b.db.Close(); err != nil {
		return fmt.Errorf("fs: close metadata database: %w", err)
	}
	return nil
}

// view runs fn in a read transaction.
func (b *Backend) view(fn func(*bbolt.Tx) error) error {
	if b.closed.Load() {
		return ErrClosed
	}
	return mapDBErr(b.db.View(fn))
}

// update runs fn in a write transaction. Concurrent calls are coalesced
// into shared transactions (group commit, see committer), so fn may be run
// more than once and must only have effects through tx and through
// variables it resets on every run.
func (b *Backend) update(fn func(*bbolt.Tx) error) error {
	if b.closed.Load() {
		return ErrClosed
	}
	err := b.commits.do(b.db, fn)
	if err == nil && b.commitFault != nil {
		err = b.commitFault()
	}
	return mapDBErr(err)
}

// updateFiles is update for transactions that publish files written
// beforehand. It additionally reports whether a failure is indeterminate:
// fn itself succeeded and the error came from committing the transaction,
// which may nonetheless have reached the disk (bbolt can fail an fdatasync
// after writing the meta page). The caller must then keep the new files
// rather than delete them; the recovery pass removes them at the next start
// if the commit was in fact lost.
func (b *Backend) updateFiles(fn func(*bbolt.Tx) error) (indeterminate bool, err error) {
	var fnOK bool
	err = b.update(func(tx *bbolt.Tx) error {
		fnOK = false
		if err := fn(tx); err != nil {
			return err
		}
		fnOK = true
		return nil
	})
	return err != nil && fnOK, err
}

func mapDBErr(err error) error {
	if errors.Is(err, berrors.ErrDatabaseNotOpen) {
		return ErrClosed
	}
	return err
}

// begin checks the context and the closed state at the start of an operation.
func (b *Backend) begin(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if b.closed.Load() {
		return ErrClosed
	}
	return nil
}

// --- paths ---

func (b *Backend) bucketDataDir(bucketID string) string {
	return filepath.Join(b.dataDir, bucketID)
}

func (b *Backend) dataPath(bucketID, dataID string) string {
	return filepath.Join(b.dataDir, bucketID, dataID[:2], dataID)
}

func (b *Backend) uploadPath(uploadID string) string {
	return filepath.Join(b.uploadDir, uploadID)
}

func (b *Backend) partPath(uploadID, file string) string {
	return filepath.Join(b.uploadDir, uploadID, file)
}

// unlinkData removes data files after the transaction that dereferenced them
// committed. Failures are logged; the recovery pass removes leftovers.
func (b *Backend) unlinkData(bucketID string, ids []string) {
	for _, id := range ids {
		if !validDataID(id) {
			continue
		}
		if err := os.Remove(b.dataPath(bucketID, id)); err != nil && !errors.Is(err, iofs.ErrNotExist) {
			b.log.Warn("fs: unlink data file", "bucket_id", bucketID, "data_id", id, "err", err)
		}
	}
}

// --- pending files ---

// pendingSet tracks final paths of files renamed into data/ or uploads/
// whose metadata transaction has not finished yet, so the recovery pass
// never mistakes them for orphans. A file is added before it is renamed into
// place and removed after its transaction finished.
type pendingSet struct {
	mu sync.Mutex
	m  map[string]int
}

func (p *pendingSet) add(path string) {
	p.mu.Lock()
	p.m[path]++
	p.mu.Unlock()
}

func (p *pendingSet) done(path string) {
	p.mu.Lock()
	if p.m[path] <= 1 {
		delete(p.m, path)
	} else {
		p.m[path]--
	}
	p.mu.Unlock()
}

func (p *pendingSet) has(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.m[path] > 0
}

// --- file helpers ---

// tmpFile is a file being written in tmp/ through a buffer. The file itself
// is created lazily, when the buffer first overflows or at finish, so empty
// objects never touch the filesystem.
type tmpFile struct {
	// cryptoID and key are set for an encrypted file: its data ID and
	// stream key (see encryptedTemp). Everything written then goes
	// through ew into f.
	cryptoID string
	key      []byte
	ew       *crypt.Writer
	dir      string
	f        *os.File
	path     string // f.Name() once created
	buf      []byte
	n        int // buffered bytes
	size     int64
	err      error // sticky error
}

const writeBufSize = 64 << 10

func (b *Backend) newTemp() *tmpFile { return &tmpFile{dir: b.tmpDir} }

// open creates the backing file if needed.
func (t *tmpFile) open() error {
	if t.err != nil || t.f != nil {
		return t.err
	}
	f, err := os.CreateTemp(t.dir, "w-*")
	if err != nil {
		t.err = fmt.Errorf("fs: create temporary file: %w", err)
		return t.err
	}
	t.f, t.path = f, f.Name()
	if t.key != nil {
		if t.ew, t.err = crypt.NewWriter(f, t.key); t.err != nil {
			t.err = fmt.Errorf("fs: encrypt data: %w", t.err)
		}
	}
	return t.err
}

func (t *tmpFile) Write(p []byte) (int, error) {
	if t.err != nil {
		return 0, t.err
	}
	written := 0
	for len(p) > 0 {
		if t.n == 0 && len(p) >= writeBufSize {
			// Large write with an empty buffer: bypass the buffer.
			if err := t.open(); err != nil {
				return written, err
			}
			n, err := t.writeDisk(p)
			written += n
			t.size += int64(n)
			if err != nil {
				t.err = fmt.Errorf("fs: write data: %w", err)
				return written, t.err
			}
			return written, nil
		}
		if t.buf == nil {
			t.buf = make([]byte, writeBufSize)
		}
		c := copy(t.buf[t.n:], p)
		t.n += c
		t.size += int64(c)
		written += c
		p = p[c:]
		if t.n == len(t.buf) {
			if err := t.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// flush writes buffered bytes to the file, creating it if needed.
func (t *tmpFile) flush() error {
	if t.err != nil || t.n == 0 {
		return t.err
	}
	if err := t.open(); err != nil {
		return err
	}
	_, err := t.writeDisk(t.buf[:t.n])
	t.n = 0
	if err != nil {
		t.err = fmt.Errorf("fs: write data: %w", err)
	}
	return t.err
}

// finish flushes, optionally fsyncs and closes the file. When nothing was
// written and keepEmpty is false no file is created (t.path stays "").
func (t *tmpFile) finish(sync, keepEmpty bool) error {
	defer func() { t.buf = nil }()
	if t.size == 0 && t.f == nil && !keepEmpty {
		return t.err
	}
	err := t.open()
	if err == nil {
		err = t.flush()
	}
	if err == nil && t.ew != nil {
		// Seals the final chunk; f stays open.
		if err = t.ew.Close(); err != nil {
			err = fmt.Errorf("fs: write data: %w", err)
		}
	}
	if err == nil && sync {
		if serr := t.f.Sync(); serr != nil {
			err = fmt.Errorf("fs: sync data: %w", serr)
		}
	}
	if t.f != nil {
		if cerr := t.f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("fs: close data file: %w", cerr)
		}
	}
	if err != nil {
		t.remove()
	}
	return err
}

// discard closes and removes the temporary file (if it was created).
func (t *tmpFile) discard() {
	if t.f != nil {
		_ = t.f.Close()
	}
	t.remove()
	t.buf = nil
}

// remove deletes the temporary file, if any.
func (t *tmpFile) remove() {
	if t.path != "" {
		_ = os.Remove(t.path)
	}
}

// place renames src to dst, creating dst's directory (and retrying once if a
// concurrent cleanup removed it), and fsyncs the directory when requested.
func (b *Backend) place(src, dst string) error {
	dir := filepath.Dir(dst)
	created := false
	if b.fsync {
		if _, err := os.Stat(dir); err != nil {
			created = true
		}
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = os.MkdirAll(dir, dirPerm); err != nil {
			continue
		}
		if err = os.Rename(src, dst); err == nil {
			break
		}
		if !errors.Is(err, iofs.ErrNotExist) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("fs: move data file into place: %w", err)
	}
	if b.fsync {
		if err := syncDir(dir); err != nil {
			return err
		}
		if created {
			// New directories (data/<bucket-id>/<xx>, uploads/<id>) must
			// be durable in their parents too.
			parent := filepath.Dir(dir)
			for _, d := range []string{parent, filepath.Dir(parent)} {
				if err := syncDir(d); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// syncDir fsyncs a directory so a rename into it is durable.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("fs: sync dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fs: sync dir: %w", err)
	}
	return nil
}

// clearDir removes every entry of dir and returns how many were removed.
func clearDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// emptyReader is the reader of zero-length objects (which have no file).
type emptyReader struct{ *bytes.Reader }

func newEmptyReader() emptyReader { return emptyReader{bytes.NewReader(nil)} }

func (emptyReader) Close() error { return nil }

// writeDisk writes p to the file, through the encryption stream when the
// file is encrypted.
func (t *tmpFile) writeDisk(p []byte) (int, error) {
	if t.ew != nil {
		return t.ew.Write(p)
	}
	return t.f.Write(p)
}
