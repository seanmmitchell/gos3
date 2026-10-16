package localstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func openDB(t *testing.T, dir string) *DB {
	t.Helper()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func must(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func wantRecords(t *testing.T, s *Store, want map[string][]byte) {
	t.Helper()
	got, err := s.Records(t.Context())
	must(t, "Records", err)
	if got == nil {
		t.Fatalf("Records returned a nil map")
	}
	if !maps.EqualFunc(got, want, bytes.Equal) {
		t.Fatalf("Records = %q, want %q", got, want)
	}
}

func wantStaged(t *testing.T, s *Store, want ...string) {
	t.Helper()
	got, err := s.ListStaged(t.Context())
	must(t, "ListStaged", err)
	if got == nil {
		t.Fatalf("ListStaged returned a nil slice")
	}
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ListStaged = %q, want %q", got, want)
	}
}

func readStaged(t *testing.T, s *Store, name string, size int64) []byte {
	t.Helper()
	r, err := s.OpenStaged(t.Context(), name, size)
	must(t, fmt.Sprintf("OpenStaged(%q, %d)", name, size), err)
	defer r.Close()
	data, err := io.ReadAll(r)
	must(t, "read staged blob", err)
	return data
}

func TestRecords(t *testing.T) {
	ctx := t.Context()
	d := openDB(t, t.TempDir())
	s := d.Namespace("account")
	wantRecords(t, s, map[string][]byte{})
	must(t, "DeleteRecords on an empty namespace", s.DeleteRecords(ctx, "x"))

	want := map[string][]byte{}
	put := func(k, v string) {
		t.Helper()
		must(t, "PutRecord "+k, s.PutRecord(ctx, k, []byte(v)))
		want[k] = []byte(v)
	}
	put("b/one", "1")
	put("b/two", "2")
	put("b/two/x", "nested")
	put("c/one", "c")
	put("v", `{"schema":1}`)
	put("empty", "")
	put("bin\x00\xff", "\x00\x01")
	wantRecords(t, s, want)

	put("b/one", "replaced")
	wantRecords(t, s, want)

	// Records returns copies.
	got, err := s.Records(ctx)
	must(t, "Records", err)
	got["b/one"][0] = 'X'
	got["new"] = []byte("x")
	wantRecords(t, s, want)

	// The value passed to PutRecord is not retained.
	val := []byte("mutable")
	must(t, "PutRecord", s.PutRecord(ctx, "m", val))
	val[0] = 'X'
	want["m"] = []byte("mutable")
	wantRecords(t, s, want)

	// A nil value is an empty record.
	must(t, "PutRecord nil", s.PutRecord(ctx, "nil", nil))
	want["nil"] = []byte{}
	wantRecords(t, s, want)

	// Prefix deletes.
	must(t, "DeleteRecords b/two", s.DeleteRecords(ctx, "b/two"))
	delete(want, "b/two")
	delete(want, "b/two/x")
	wantRecords(t, s, want)
	must(t, "DeleteRecords of a missing prefix", s.DeleteRecords(ctx, "zz"))
	wantRecords(t, s, want)
	must(t, "DeleteRecords exact key", s.DeleteRecords(ctx, "v"))
	delete(want, "v")
	wantRecords(t, s, want)

	// Namespaces are independent.
	o := d.Namespace("gateway")
	wantRecords(t, o, map[string][]byte{})
	must(t, "PutRecord in another namespace", o.PutRecord(ctx, "b/one", []byte("other")))
	must(t, "DeleteRecords all in another namespace", o.DeleteRecords(ctx, ""))
	wantRecords(t, o, map[string][]byte{})
	wantRecords(t, s, want)

	// "" deletes every record of the namespace.
	must(t, "DeleteRecords all", s.DeleteRecords(ctx, ""))
	wantRecords(t, s, map[string][]byte{})

	// Invalid keys.
	for _, k := range []string{"", strings.Repeat("k", bbolt.MaxKeySize+1)} {
		if err := s.PutRecord(ctx, k, []byte("x")); !errors.Is(err, storage.ErrInvalidName) {
			t.Errorf("PutRecord(%d-byte key) = %v, want ErrInvalidName", len(k), err)
		}
	}
	must(t, "PutRecord of a MaxKeySize key", s.PutRecord(ctx, strings.Repeat("k", bbolt.MaxKeySize), []byte("x")))
}

func TestStaged(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	d := openDB(t, dir)
	s := d.Namespace("gateway")
	wantStaged(t, s)

	_, err := s.OpenStaged(ctx, "missing", 1)
	if !errors.Is(err, storage.ErrPartNotFound) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenStaged of a missing blob = %v, want ErrPartNotFound and fs.ErrNotExist", err)
	}
	must(t, "DeleteStaged of a missing blob", s.DeleteStaged(ctx, "missing"))

	data := bytes.Repeat([]byte("0123456789abcdef"), 4096+3)
	must(t, "PutStaged", s.PutStaged(ctx, "up1-00001-x", bytes.NewReader(data), int64(len(data))))
	if p := filepath.Join(dir, "staged", "gateway", "up1-00001-x"); !fileExists(p) {
		t.Fatalf("staged blob not at %s", p)
	}
	if got := readStaged(t, s, "up1-00001-x", int64(len(data))); !bytes.Equal(got, data) {
		t.Fatalf("staged content mismatch: %d bytes, want %d", len(got), len(data))
	}

	// The reader seeks.
	r, err := s.OpenStaged(ctx, "up1-00001-x", int64(len(data)))
	must(t, "OpenStaged", err)
	_, err = r.Seek(100, io.SeekStart)
	must(t, "Seek", err)
	buf := make([]byte, 10)
	_, err = io.ReadFull(r, buf)
	must(t, "read after Seek", err)
	if !bytes.Equal(buf, data[100:110]) {
		t.Errorf("read after Seek = %q, want %q", buf, data[100:110])
	}
	must(t, "Close reader", r.Close())

	// Only the first size bytes of r are stored.
	must(t, "PutStaged of a prefix", s.PutStaged(ctx, "prefix", bytes.NewReader(data), 10))
	if got := readStaged(t, s, "prefix", 10); !bytes.Equal(got, data[:10]) {
		t.Errorf("staged prefix = %q, want %q", got, data[:10])
	}

	// Size mismatch.
	for _, size := range []int64{int64(len(data)) - 1, int64(len(data)) + 1, 0} {
		if r, err := s.OpenStaged(ctx, "up1-00001-x", size); err == nil {
			r.Close()
			t.Errorf("OpenStaged with size %d of a %d-byte blob succeeded", size, len(data))
		} else if errors.Is(err, storage.ErrPartNotFound) {
			t.Errorf("OpenStaged with a wrong size = %v, want an error other than ErrPartNotFound", err)
		}
	}

	// Empty blobs.
	must(t, "PutStaged empty", s.PutStaged(ctx, "empty", bytes.NewReader(nil), 0))
	if got := readStaged(t, s, "empty", 0); len(got) != 0 {
		t.Errorf("empty blob reads %q", got)
	}

	// Replacing.
	must(t, "PutStaged replace", s.PutStaged(ctx, "prefix", strings.NewReader("new content"), 11))
	if got := readStaged(t, s, "prefix", 11); string(got) != "new content" {
		t.Errorf("replaced blob = %q", got)
	}

	// A short reader fails and leaves the previous blob and no temporary file.
	err = s.PutStaged(ctx, "prefix", strings.NewReader("short"), 100)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("PutStaged from a short reader = %v, want io.ErrUnexpectedEOF", err)
	}
	if got := readStaged(t, s, "prefix", 11); string(got) != "new content" {
		t.Errorf("blob after a failed replace = %q, want the previous content", got)
	}
	err = s.PutStaged(ctx, "new", strings.NewReader("short"), 100)
	if err == nil {
		t.Errorf("PutStaged from a short reader succeeded")
	}
	if _, err := s.OpenStaged(ctx, "new", 5); !errors.Is(err, storage.ErrPartNotFound) {
		t.Errorf("OpenStaged after a failed PutStaged = %v, want ErrPartNotFound", err)
	}
	if n := countTemps(t, filepath.Join(dir, "staged", "gateway")); n != 0 {
		t.Errorf("%d temporary files left after failed writes", n)
	}

	// A canceled context fails without writing.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.PutStaged(cctx, "canceled", bytes.NewReader(data), int64(len(data))); !errors.Is(err, context.Canceled) {
		t.Errorf("PutStaged with a canceled context = %v, want context.Canceled", err)
	}

	wantStaged(t, s, "empty", "prefix", "up1-00001-x")
	wantStaged(t, d.Namespace("other"))

	// Files that are not blobs are not listed.
	nsDir := filepath.Join(dir, "staged", "gateway")
	for _, name := range []string{tmpPrefix + "123", "with space", "con"} {
		must(t, "write foreign file", os.WriteFile(filepath.Join(nsDir, name), []byte("x"), 0o600))
	}
	must(t, "mkdir", os.Mkdir(filepath.Join(nsDir, "subdir"), 0o700))
	wantStaged(t, s, "empty", "prefix", "up1-00001-x")

	must(t, "DeleteStaged", s.DeleteStaged(ctx, "prefix"))
	must(t, "DeleteStaged again", s.DeleteStaged(ctx, "prefix"))
	wantStaged(t, s, "empty", "up1-00001-x")
	if _, err := s.OpenStaged(ctx, "prefix", 11); !errors.Is(err, storage.ErrPartNotFound) {
		t.Errorf("OpenStaged of a deleted blob = %v, want ErrPartNotFound", err)
	}
}

func TestInvalidNames(t *testing.T) {
	ctx := t.Context()
	d := openDB(t, t.TempDir())
	for _, ns := range []string{"", "Account", "a/b", "..", ".", "a_b", "a b", strings.Repeat("a", 33), "é"} {
		s := d.Namespace(ns)
		errs := map[string]error{
			"PutRecord":     s.PutRecord(ctx, "k", nil),
			"DeleteRecords": s.DeleteRecords(ctx, ""),
			"PutStaged":     s.PutStaged(ctx, "x", strings.NewReader("x"), 1),
			"DeleteStaged":  s.DeleteStaged(ctx, "x"),
		}
		_, errs["Records"] = s.Records(ctx)
		_, errs["OpenStaged"] = s.OpenStaged(ctx, "x", 1)
		_, errs["ListStaged"] = s.ListStaged(ctx)
		for op, err := range errs {
			if !errors.Is(err, storage.ErrInvalidName) {
				t.Errorf("namespace %q: %s = %v, want ErrInvalidName", ns, op, err)
			}
		}
	}
	for _, ns := range []string{"a", "account", "gateway-2", strings.Repeat("z", 32), "0"} {
		must(t, "PutRecord in namespace "+ns, d.Namespace(ns).PutRecord(ctx, "k", nil))
	}

	s := d.Namespace("ns")
	invalid := []string{
		"", ".", "..", ".hidden", tmpPrefix + "x", "a/b", "/a", "a/", `a\b`, "a..b", "..a", "a.", "a b", "a:b",
		"é", "a\x00", strings.Repeat("n", maxNameLen+1),
		"con", "CON", "nul.txt", "Aux", "prn.x.y", "com1", "LPT9", "lpt1.part",
	}
	for _, name := range invalid {
		if err := s.PutStaged(ctx, name, strings.NewReader("x"), 1); !errors.Is(err, storage.ErrInvalidName) {
			t.Errorf("PutStaged(%q) = %v, want ErrInvalidName", name, err)
		}
		if _, err := s.OpenStaged(ctx, name, 1); !errors.Is(err, storage.ErrInvalidName) {
			t.Errorf("OpenStaged(%q) = %v, want ErrInvalidName", name, err)
		}
		if err := s.DeleteStaged(ctx, name); !errors.Is(err, storage.ErrInvalidName) {
			t.Errorf("DeleteStaged(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	valid := []string{
		// No two differ only in case: they would clash on case-insensitive
		// file systems (see validName).
		"a", "-x", "_y", "a.b.c", "Zz09-_.x", "console", "com0", "COM10", "lptx", "x.con", "nul-1",
		strings.Repeat("n", maxNameLen),
		"dGVzdA_-AbC-00001-aGVsbG8", // gateway part names: <upload id>-<part>-<random>, base64url
	}
	for _, name := range valid {
		must(t, fmt.Sprintf("PutStaged(%q)", name), s.PutStaged(ctx, name, strings.NewReader("x"), 1))
	}
	got, err := s.ListStaged(ctx)
	must(t, "ListStaged", err)
	if want := slices.Sorted(slices.Values(valid)); !slices.Equal(got, want) {
		t.Errorf("ListStaged = %q, want %q", got, want)
	}
}

func TestReopen(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "s3", "backing") // created by Open
	d, err := Open(dir, Options{})
	must(t, "Open", err)
	a, g := d.Namespace("account"), d.Namespace("gateway")
	must(t, "PutRecord", a.PutRecord(ctx, "b/bucket", []byte(`{"kind":"native"}`)))
	must(t, "PutRecord", a.PutRecord(ctx, "v", []byte(`{"schema":1}`)))
	must(t, "PutRecord", g.PutRecord(ctx, "u/1", []byte("upload")))
	must(t, "PutRecord", g.PutRecord(ctx, "u/2", []byte("gone")))
	must(t, "DeleteRecords", g.DeleteRecords(ctx, "u/2"))
	must(t, "PutStaged", g.PutStaged(ctx, "part-1", strings.NewReader("part one"), 8))
	must(t, "PutStaged", g.PutStaged(ctx, "part-2", strings.NewReader("part two"), 8))
	must(t, "DeleteStaged", g.DeleteStaged(ctx, "part-2"))
	must(t, "Close", d.Close())
	must(t, "Close again", d.Close())

	// A closed DB's stores fail.
	if _, err := a.Records(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Records after Close = %v, want ErrClosed", err)
	}
	if err := g.PutStaged(ctx, "late", strings.NewReader("x"), 1); !errors.Is(err, ErrClosed) {
		t.Errorf("PutStaged after Close = %v, want ErrClosed", err)
	}
	if _, err := g.ListStaged(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("ListStaged after Close = %v, want ErrClosed", err)
	}

	// A crash in the middle of a staged write leaves a temporary file.
	tmp := filepath.Join(dir, "staged", "gateway", tmpPrefix+"12345")
	must(t, "write temporary file", os.WriteFile(tmp, []byte("partial"), 0o600))

	d = openDB(t, dir)
	a, g = d.Namespace("account"), d.Namespace("gateway")
	wantRecords(t, a, map[string][]byte{"b/bucket": []byte(`{"kind":"native"}`), "v": []byte(`{"schema":1}`)})
	wantRecords(t, g, map[string][]byte{"u/1": []byte("upload")})
	wantStaged(t, g, "part-1")
	if got := readStaged(t, g, "part-1", 8); string(got) != "part one" {
		t.Errorf("staged blob after reopen = %q", got)
	}
	if fileExists(tmp) {
		t.Errorf("Open left the temporary file %s", tmp)
	}

	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{
			dir:                                     0o700,
			filepath.Join(dir, "staged"):            0o700,
			filepath.Join(dir, "state.db"):          0o600,
			filepath.Join(dir, "staged", "gateway"): 0o700,
		} {
			fi, err := os.Stat(p)
			must(t, "Stat", err)
			if got := fi.Mode().Perm(); got&0o077 != 0 || got&want != want {
				t.Errorf("%s has mode %v, want %v", p, got, want)
			}
		}
	}
}

func TestOpenTightensPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits on Windows")
	}
	dir := filepath.Join(t.TempDir(), "records")
	must(t, "mkdir", os.Mkdir(dir, 0o755))
	must(t, "chmod", os.Chmod(dir, 0o755)) // regardless of the umask
	openDB(t, dir)
	fi, err := os.Stat(dir)
	must(t, "Stat", err)
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("Open left %s with mode %v, want 0700", dir, perm)
	}
}

func TestSecondOpenFailsFast(t *testing.T) {
	dir := t.TempDir()
	d := openDB(t, dir)
	start := time.Now()
	d2, err := Open(dir, Options{})
	if err == nil {
		d2.Close()
		t.Fatal("second Open of a locked state database succeeded")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), "state.db") {
		t.Errorf("error %q does not name the database", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("second Open took %v to fail, want about %v", elapsed, DefaultLockTimeout)
	}
	// A short LockTimeout is honoured.
	start = time.Now()
	if _, err := Open(dir, Options{LockTimeout: 50 * time.Millisecond}); !errors.Is(err, ErrLocked) {
		t.Fatalf("Open with LockTimeout = %v, want ErrLocked", err)
	}
	if elapsed := time.Since(start); elapsed > DefaultLockTimeout {
		t.Errorf("Open with a 50ms LockTimeout took %v", elapsed)
	}
	// Once closed, the directory opens again.
	must(t, "Close", d.Close())
	d3, err := Open(dir, Options{})
	must(t, "Open after Close", err)
	must(t, "Close", d3.Close())
}

func TestCanceledContext(t *testing.T) {
	d := openDB(t, t.TempDir())
	s := d.Namespace("ns")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	errs := map[string]error{
		"PutRecord":     s.PutRecord(ctx, "k", nil),
		"DeleteRecords": s.DeleteRecords(ctx, ""),
		"PutStaged":     s.PutStaged(ctx, "x", strings.NewReader("x"), 1),
		"DeleteStaged":  s.DeleteStaged(ctx, "x"),
	}
	_, errs["Records"] = s.Records(ctx)
	_, errs["OpenStaged"] = s.OpenStaged(ctx, "x", 1)
	_, errs["ListStaged"] = s.ListStaged(ctx)
	for op, err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s with a canceled context = %v, want context.Canceled", op, err)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	ctx := t.Context()
	d := openDB(t, t.TempDir())
	const workers, rounds = 8, 20
	var wg sync.WaitGroup
	errc := make(chan error, workers)
	for w := range workers {
		// Two workers share each namespace.
		s := d.Namespace(fmt.Sprintf("ns%d", w/2))
		wg.Go(func() {
			if err := worker(ctx, s, w, rounds); err != nil {
				errc <- fmt.Errorf("worker %d: %w", w, err)
			}
		})
	}
	// Readers run alongside.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := range 2 {
		readers.Go(func() {
			s := d.Namespace(fmt.Sprintf("ns%d", r))
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := s.Records(ctx); err != nil {
					errc <- err
					return
				}
				if _, err := s.ListStaged(ctx); err != nil {
					errc <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	// Each worker kept its last round's record and blob.
	for ns := range workers / 2 {
		s := d.Namespace(fmt.Sprintf("ns%d", ns))
		want := map[string][]byte{}
		var blobs []string
		for _, w := range []int{2 * ns, 2*ns + 1} {
			want[fmt.Sprintf("w%d/r%03d", w, rounds-1)] = fmt.Appendf(nil, "value %d/%d", w, rounds-1)
			blobs = append(blobs, fmt.Sprintf("w%d-r%d", w, rounds-1))
		}
		wantRecords(t, s, want)
		wantStaged(t, s, blobs...)
	}
}

// worker writes, reads back and deletes its own records and blobs in s;
// it keeps those of its last round.
func worker(ctx context.Context, s *Store, w, rounds int) error {
	for r := range rounds {
		key := fmt.Sprintf("w%d/r%03d", w, r)
		val := fmt.Appendf(nil, "value %d/%d", w, r)
		if err := s.PutRecord(ctx, key, val); err != nil {
			return err
		}
		name := fmt.Sprintf("w%d-r%d", w, r)
		data := bytes.Repeat(val, 100+r)
		if err := s.PutStaged(ctx, name, bytes.NewReader(data), int64(len(data))); err != nil {
			return err
		}
		recs, err := s.Records(ctx)
		if err != nil {
			return err
		}
		if !bytes.Equal(recs[key], val) {
			return fmt.Errorf("record %s = %q, want %q", key, recs[key], val)
		}
		rc, err := s.OpenStaged(ctx, name, int64(len(data)))
		if err != nil {
			return err
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		if !bytes.Equal(got, data) {
			return fmt.Errorf("blob %s: content mismatch", name)
		}
		if r > 0 {
			// The previous round's record and blob.
			prev := fmt.Sprintf("w%d/r%03d", w, r-1)
			if err := s.DeleteRecords(ctx, prev); err != nil {
				return err
			}
			if err := s.DeleteStaged(ctx, fmt.Sprintf("w%d-r%d", w, r-1)); err != nil {
				return err
			}
		}
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func countTemps(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, "ReadDir", err)
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			n++
		}
	}
	return n
}
