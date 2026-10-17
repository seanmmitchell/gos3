// Package localstate keeps the records of an s3 backing that has no remote
// home (internal/storage/s3account) in a local, durable state database.
//
// An s3 backing's own records are the bucket registry (which gos3 bucket
// is of which kind and lives where), the configuration documents of native
// buckets and the multipart state of native buckets (upload and part
// records, small staged parts, overlay records). With a home they live in
// aux namespaces of the home's s3store (s3store.AuxState); without one,
// they live here, in the backing's directory under the system directory.
// The layout mirrors the home's: one namespace per aux namespace (e.g.
// "account" for the registry, the native store's namespace for its state),
// each a Store with exactly the method set of s3store.AuxStore and
// native.StateStore.
//
// On disk (dir is the backing's local records directory, mode 0700):
//
//	<dir>/state.db                 bbolt: one bucket per namespace, key -> value
//	<dir>/staged/<ns>/<name>       staged blobs, one file each
//	<dir>/staged/<ns>/.tmp-*       staged blobs being written
//
// Every record change is a bbolt transaction, synced to disk before it
// returns. A staged blob is written to a temporary file that is synced and
// then renamed into place (and the directory synced, where the OS allows),
// so a blob exists completely or not at all. Temporary files left by a
// crash are removed by Open.
//
// The database is locked while open: a second Open of the same directory,
// from this or another process, fails after a short wait with ErrLocked,
// so two servers never share one backing's records.
package localstate

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
)

// DefaultLockTimeout is how long Open waits for another holder of the
// state database's lock before failing with ErrLocked.
const DefaultLockTimeout = time.Second

const (
	dbFile    = "state.db"
	stagedDir = "staged"
	tmpPrefix = ".tmp-" // temporary staged files; never a valid blob name

	dirPerm  = 0o700 // the records name every bucket and hold upload state
	filePerm = 0o600
)

var (
	// ErrLocked is returned by Open when the state database is in use by
	// another DB (another gos3 process, or this one).
	ErrLocked = errors.New("localstate: state database is locked by another process")
	// ErrClosed is returned by a Store whose DB was closed.
	ErrClosed = errors.New("localstate: state database is closed")
)

// Options configures Open.
type Options struct {
	// Logger receives non-fatal events (removed temporary files,
	// tightened permissions). nil discards them.
	Logger *slog.Logger
	// LockTimeout bounds the wait for the state database's lock (0 =
	// DefaultLockTimeout).
	LockTimeout time.Duration
}

// DB is an open state database. Create it with Open; it is safe for
// concurrent use.
type DB struct {
	dir string
	db  *bbolt.DB
	log *slog.Logger

	// mu guards closed against the operations in flight: they hold it
	// shared so Close waits for them.
	mu     sync.RWMutex
	closed bool
}

// Open opens the state database in dir, creating dir (mode 0700), the
// database and the staged blob directory as needed, and removes the
// temporary files of staged blobs whose write did not finish. It fails
// with ErrLocked when another DB holds the database.
func Open(dir string, opts Options) (*DB, error) {
	if dir == "" {
		return nil, errors.New("localstate: empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("localstate: resolve %s: %w", dir, err)
	}
	d := &DB{dir: abs, log: opts.Logger}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	timeout := opts.LockTimeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	existing := existingAncestor(abs)
	for _, p := range []string{abs, filepath.Join(abs, stagedDir)} {
		if err := os.MkdirAll(p, dirPerm); err != nil {
			return nil, fmt.Errorf("localstate: create %s: %w", p, err)
		}
		d.tightenPerm(p)
	}
	path := filepath.Join(abs, dbFile)
	db, err := bbolt.Open(path, filePerm, &bbolt.Options{
		Timeout:        timeout,
		FreelistType:   bbolt.FreelistMapType,
		NoFreelistSync: true,
	})
	if err != nil {
		if errors.Is(err, berrors.ErrTimeout) {
			return nil, fmt.Errorf("%w: %s (is another gos3 using this directory?)", ErrLocked, path)
		}
		return nil, fmt.Errorf("localstate: open %s: %w", path, err)
	}
	d.db = db
	d.tightenPerm(path)
	// Make the new directory entries (state.db, staged/ and the
	// directories MkdirAll created) durable before anything is stored in
	// them.
	err = syncDir(abs)
	for p := abs; err == nil && p != existing; p = filepath.Dir(p) {
		err = syncDir(filepath.Dir(p))
	}
	// The database lock is held: no other DB writes staged files now.
	if err == nil {
		err = d.removeTemps()
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the database, waiting for operations in flight. Stores of
// a closed DB answer ErrClosed. Closing twice is a no-op.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("localstate: close: %w", err)
	}
	return nil
}

// Namespace returns the store of namespace ns: 1-32 characters from
// [a-z0-9-]. The methods of a Store with an invalid namespace fail with an
// error wrapping storage.ErrInvalidName. Namespaces are independent; a
// namespace exists once something is stored in it.
func (d *DB) Namespace(ns string) *Store {
	s := &Store{d: d, ns: ns, dir: filepath.Join(d.dir, stagedDir, ns)}
	if !validNamespace(ns) {
		s.err = invalidName("namespace", ns)
	}
	return s
}

// validNamespace reports whether ns is 1-32 characters from [a-z0-9-].
func validNamespace(ns string) bool {
	if ns == "" || len(ns) > 32 {
		return false
	}
	for i := 0; i < len(ns); i++ {
		c := ns[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// enter starts an operation: it holds d.mu shared until the returned func
// is called, and fails once the DB is closed.
func (d *DB) enter() (exit func(), err error) {
	d.mu.RLock()
	if d.closed {
		d.mu.RUnlock()
		return nil, ErrClosed
	}
	return d.mu.RUnlock, nil
}

// removeTemps removes the temporary files of unfinished staged writes in
// every namespace directory.
func (d *DB) removeTemps() error {
	root := filepath.Join(d.dir, stagedDir)
	nss, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("localstate: read %s: %w", root, err)
	}
	n := 0
	for _, ns := range nss {
		if !ns.IsDir() {
			continue
		}
		dir := filepath.Join(root, ns.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("localstate: read %s: %w", dir, err)
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), tmpPrefix) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("localstate: remove temporary file: %w", err)
			}
			n++
		}
	}
	if n > 0 {
		d.log.Info("localstate: removed temporary files of unfinished staged writes", "dir", root, "count", n)
	}
	return nil
}

// tightenPerm removes the group and other permission bits of path when it
// exists with them set: MkdirAll and bbolt.Open apply the modes only when
// they create a path, and a directory pre-created by an operator or a
// container volume would otherwise stay readable by other local users. A
// failure is logged, not fatal. Windows has no mode bits to fix.
func (d *DB) tightenPerm(path string) {
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
		d.log.Warn("localstate: cannot restrict permissions; other local users may read it", "path", path, "mode", perm.String(), "err", err)
		return
	}
	d.log.Warn("localstate: restricted permissions that allowed access by other local users", "path", path, "was", perm.String(), "now", (perm &^ 0o077).String())
}

// existingAncestor returns the longest prefix of the absolute path p that
// exists (p itself when it exists).
func existingAncestor(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// syncDir fsyncs a directory so that a rename or creation in it is
// durable. Windows cannot open directories for syncing (and NTFS journals
// renames), so it is a no-op there.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("localstate: sync %s: %w", dir, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("localstate: sync %s: %w", dir, err)
	}
	return nil
}
