package fs

import (
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/storagetest"
)

// TestConformance runs the backend-agnostic conformance suite against the
// filesystem backend, each subtest in a fresh root directory.
func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		b, err := Open(t.TempDir(), Options{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return b
	})
}

// TestConformanceDurable runs the suite with FsyncData enabled, which takes
// the directory-sync code paths.
func TestConformanceDurable(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		b, err := Open(t.TempDir(), Options{FsyncData: true})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return b
	})
}
