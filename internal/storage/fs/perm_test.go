//go:build !windows

package fs

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenTightensPermissions is the regression test for loose modes on a
// pre-existing (restored, pre-created) system directory and meta.db: Open
// removes the group/other bits and logs a warning.
func TestOpenTightensPermissions(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	sys := filepath.Join(dir, ".gos3")
	loose := map[string]os.FileMode{
		sys:                             0o755,
		filepath.Join(sys, "data"):      0o775,
		filepath.Join(sys, "uploads"):   0o755,
		filepath.Join(sys, "tmp"):       0o777,
		filepath.Join(sys, "orphans"):   0o750,
		filepath.Join(sys, "meta.db"):   0o644,
		filepath.Join(dir, "elsewhere"): 0o755, // outside .gos3: untouched
	}
	for _, d := range []string{filepath.Join(sys, "orphans"), filepath.Join(dir, "elsewhere")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for p, m := range loose {
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}

	var logBuf bytes.Buffer
	b2, err := Open(dir, Options{Logger: slog.New(slog.NewTextHandler(&logBuf, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b2.Close() })

	for p, was := range loose {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want := was &^ 0o077
		if filepath.Base(p) == "elsewhere" {
			want = was
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %v, want %v", p, got, want)
		}
	}
	if !strings.Contains(logBuf.String(), "restricted permissions") || !strings.Contains(logBuf.String(), "meta.db") {
		t.Errorf("no warning logged: %s", logBuf.String())
	}

	// Already private: nothing to change, nothing logged.
	if err := b2.Close(); err != nil {
		t.Fatal(err)
	}
	logBuf.Reset()
	b3, err := Open(dir, Options{Logger: slog.New(slog.NewTextHandler(&logBuf, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b3.Close() })
	if strings.Contains(logBuf.String(), "permissions") {
		t.Errorf("unexpected warning: %s", logBuf.String())
	}
}
