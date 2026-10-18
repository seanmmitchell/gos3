package localstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// maxNameLen bounds staged blob names (file names are limited to 255
// bytes on common file systems).
const maxNameLen = 200

// Store is one namespace of a DB: records (key/value pairs in the bbolt
// bucket named after the namespace) and staged blobs (files under
// staged/<ns>/). It implements s3store.AuxStore and native.StateStore and
// is safe for concurrent use. Get it with DB.Namespace.
//
// A reader returned by OpenStaged keeps reading its blob after the blob
// is deleted or replaced, except on Windows, where deleting or replacing
// a blob fails while a reader of it is open. (The stores' users delete
// staged blobs best effort and remove unreferenced ones when they open.)
type Store struct {
	d   *DB
	ns  string
	dir string // staged blob directory
	err error  // invalid namespace
}

// begin checks ctx and the store and enters the DB (see DB.enter).
func (s *Store) begin(ctx context.Context) (exit func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.d.enter()
}

// Records returns every record of the namespace (key -> value); an empty
// map when there is none.
func (s *Store) Records(ctx context.Context) (map[string][]byte, error) {
	exit, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer exit()
	out := make(map[string][]byte)
	err = s.d.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(s.ns))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			out[string(k)] = append([]byte{}, v...) // v is only valid in tx
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("localstate: read %s records: %w", s.ns, err)
	}
	return out, nil
}

// PutRecord creates or replaces a record durably. The key must be 1 to
// bbolt.MaxKeySize bytes (storage.ErrInvalidName otherwise).
func (s *Store) PutRecord(ctx context.Context, key string, val []byte) error {
	if key == "" || len(key) > bbolt.MaxKeySize {
		return invalidName("record key", key)
	}
	exit, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer exit()
	err = s.d.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(s.ns))
		if err != nil {
			return err
		}
		if val == nil {
			val = []byte{}
		}
		return b.Put([]byte(key), val)
	})
	if err != nil {
		return fmt.Errorf("localstate: put %s record %q: %w", s.ns, key, err)
	}
	return nil
}

// DeleteRecords deletes every record whose key starts with prefix ("" =
// every record of the namespace) durably. None is not an error.
func (s *Store) DeleteRecords(ctx context.Context, prefix string) error {
	exit, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer exit()
	p := []byte(prefix)
	// matching collects the keys with the prefix (copies: keys are only
	// valid in their transaction). Deleting while iterating a bbolt cursor
	// can skip keys, so deletion is a second pass.
	matching := func(b *bbolt.Bucket) [][]byte {
		var keys [][]byte
		c := b.Cursor()
		for k, _ := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, _ = c.Next() {
			keys = append(keys, bytes.Clone(k))
		}
		return keys
	}
	// A read-only pass first: deleting nothing costs no write (and sync).
	found := false
	err = s.d.db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket([]byte(s.ns)); b != nil {
			k, _ := b.Cursor().Seek(p)
			found = k != nil && bytes.HasPrefix(k, p)
		}
		return nil
	})
	if err != nil || !found {
		return err
	}
	err = s.d.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(s.ns))
		if b == nil {
			return nil
		}
		for _, k := range matching(b) {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("localstate: delete %s records %q: %w", s.ns, prefix, err)
	}
	return nil
}

// validName reports whether name can be a staged blob name: 1 to
// maxNameLen characters from [A-Za-z0-9._-], neither starting with '.'
// (so neither "." nor ".." nor a temporary file's name) nor ending with
// '.' (Windows drops trailing dots), without "..", and not a name Windows
// reserves for devices, so that every valid name is stored verbatim on
// every OS.
//
// A blob's file is named after it, so on case-insensitive file systems
// (the Windows and macOS defaults) names that differ only in case denote
// the same blob. The stores' users name blobs with random base64url
// strings, for which such a clash is practically impossible.
func validName(name string) bool {
	if name == "" || len(name) > maxNameLen || name[0] == '.' || name[len(name)-1] == '.' ||
		strings.Contains(name, "..") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return !reservedOnWindows(name)
}

// reservedOnWindows reports whether name, up to its first '.', is a DOS
// device name (CON, PRN, AUX, NUL, COM1-9, LPT1-9; any case), which
// Windows does not allow as a file name.
func reservedOnWindows(name string) bool {
	base, _, _ := strings.Cut(strings.ToUpper(name), ".")
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) &&
		base[3] >= '1' && base[3] <= '9'
}

// checkName returns an error wrapping storage.ErrInvalidName unless
// validName(name).
func checkName(name string) error {
	if !validName(name) {
		return invalidName("staged blob name", name)
	}
	return nil
}

func invalidName(what, name string) error {
	if len(name) > 64 {
		name = name[:64] + "..."
	}
	return fmt.Errorf("%w: localstate %s %q", storage.ErrInvalidName, what, name)
}

// PutStaged stores the size bytes of r as the staged blob name, replacing
// it if it exists. The blob is durable when PutStaged returns nil; on an
// error the previous blob (if any) is unchanged.
func (s *Store) PutStaged(ctx context.Context, name string, r io.ReaderAt, size int64) error {
	if err := checkName(name); err != nil {
		return err
	}
	if size < 0 {
		return fmt.Errorf("localstate: staged blob %q: negative size %d", name, size)
	}
	exit, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer exit()
	if err := s.mkdir(); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, tmpPrefix+"*")
	if err != nil {
		return fmt.Errorf("localstate: stage %q: %w", name, err)
	}
	tmp := f.Name()
	err = writeSynced(ctx, f, r, size)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(s.dir, name))
	}
	if err == nil {
		err = syncDir(s.dir)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("localstate: stage %q: %w", name, err)
	}
	return nil
}

// writeSynced copies r[0:size] to f and syncs f.
func writeSynced(ctx context.Context, f *os.File, r io.ReaderAt, size int64) error {
	n, err := io.Copy(f, &ctxReader{ctx: ctx, r: io.NewSectionReader(r, 0, size)})
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("read %d of %d bytes: %w", n, size, io.ErrUnexpectedEOF)
	}
	return f.Sync()
}

// ctxReader stops a copy once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// mkdir creates the namespace's staged directory (durably: the parent is
// synced when it is new).
func (s *Store) mkdir() error {
	if _, err := os.Stat(s.dir); err == nil {
		return nil
	}
	if err := os.Mkdir(s.dir, dirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("localstate: create %s: %w", s.dir, err)
	}
	return syncDir(filepath.Dir(s.dir))
}

// OpenStaged opens the staged blob name, which must hold size bytes. A
// missing blob is an error wrapping storage.ErrPartNotFound and
// fs.ErrNotExist.
func (s *Store) OpenStaged(ctx context.Context, name string, size int64) (io.ReadSeekCloser, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	exit, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer exit()
	f, err := os.Open(filepath.Join(s.dir, name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: localstate staged blob %q: %w", storage.ErrPartNotFound, name, err)
		}
		return nil, fmt.Errorf("localstate: open staged blob %q: %w", name, err)
	}
	fi, err := f.Stat()
	if err == nil && fi.Size() != size {
		err = fmt.Errorf("has %d bytes, want %d", fi.Size(), size)
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("localstate: staged blob %q: %w", name, err)
	}
	return f, nil
}

// DeleteStaged deletes the staged blob name; a missing one is not an
// error. The deletion is not synced: a blob that reappears after a crash
// is unreferenced and removed by its owner's cleanup (see ListStaged).
func (s *Store) DeleteStaged(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	exit, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer exit()
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("localstate: delete staged blob %q: %w", name, err)
	}
	return nil
}

// ListStaged returns the names of the namespace's staged blobs, sorted
// (an empty slice when there is none). Temporary files and files with
// names no blob can have are left out.
func (s *Store) ListStaged(ctx context.Context) ([]string, error) {
	exit, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer exit()
	entries, err := os.ReadDir(s.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("localstate: list staged blobs of %s: %w", s.ns, err)
	}
	out := []string{}
	for _, e := range entries { // sorted by name
		if e.Type().IsRegular() && validName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
