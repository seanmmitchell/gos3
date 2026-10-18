package restic

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// zeroCheck counts the bytes written to it and whether they were all zero.
type zeroCheck struct {
	n       int64
	nonZero bool
}

func (z *zeroCheck) Write(p []byte) (int, error) {
	z.n += int64(len(p))
	if !z.nonZero && len(bytes.Trim(p, "\x00")) > 0 {
		z.nonZero = true
	}
	return len(p), nil
}

func TestMakePlaceholder(t *testing.T) {
	// A modification time every file system tested keeps exactly
	// (Windows: 100 ns).
	mtime := time.Date(2024, 5, 6, 7, 8, 9, 123456700, time.UTC)
	const size = 64 << 20
	path := filepath.Join(t.TempDir(), "new", "dir", "file")
	if err := MakePlaceholder(path, size, mtime); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != size || !fi.ModTime().Equal(mtime) {
		t.Fatalf("placeholder size %d, mtime %v; want %d, %v", fi.Size(), fi.ModTime(), size, mtime)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var z zeroCheck
	_, err = io.Copy(&z, f)
	_ = f.Close()
	if err != nil || z.n != size || z.nonZero {
		t.Fatalf("placeholder content: %d bytes, non-zero %v, %v", z.n, z.nonZero, err)
	}
	switch allocated, sparse, known := allocation(t, path); {
	case !known:
		t.Logf("cannot tell whether %s is sparse on %s", path, runtime.GOOS)
	case !sparse || allocated >= size:
		if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
			t.Fatalf("placeholder not sparse: %d bytes allocated (sparse flag %v)", allocated, sparse)
		}
		t.Logf("placeholder not sparse on %s: %d bytes allocated", runtime.GOOS, allocated)
	}

	// A placeholder replaces a file's content; an empty one is empty.
	if err := os.WriteFile(path, []byte("real content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MakePlaceholder(path, 3, mtime); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, []byte{0, 0, 0}) {
		t.Fatalf("replaced placeholder = %q, %v", got, err)
	}
	if err := MakePlaceholder(path, 0, mtime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 || !fi.ModTime().Equal(mtime.Add(time.Second)) {
		t.Fatalf("empty placeholder: %v %v", fi, err)
	}
	if err := MakePlaceholder(path, -1, mtime); err == nil {
		t.Fatal("negative size accepted")
	}
}
