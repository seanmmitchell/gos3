package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
)

// TestSystemDirPermissionsTightened: a system directory created or
// restored with group/other access (and the secret files in it) is made
// private at startup.
func TestSystemDirPermissionsTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits on Windows (see config.RestrictDir)")
	}
	data := t.TempDir()
	sys := filepath.Join(data, config.SystemDirName)
	if err := os.MkdirAll(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(sys, config.SettingsFileName)
	if err := os.WriteFile(settings, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(settings, 0o644); err != nil {
		t.Fatal(err)
	}
	tightenSystemPerms(sys, testLogger(t))
	for p, want := range map[string]os.FileMode{sys: 0o700, settings: 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %v, want %v", p, got, want)
		}
	}
}
