// Package storagetest is the conformance test suite every storage.Backend
// implementation must pass.
//
// Usage from a backend package:
//
//	func TestConformance(t *testing.T) {
//		storagetest.Run(t, func(t *testing.T) storage.Backend { return New() })
//	}
//
// CONTRACT STUB: the signature of Run is fixed; the suite is owned by the
// storage-conformance work item (see docs/ARCHITECTURE.md).
package storagetest

import (
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Factory returns a fresh, empty backend for one subtest. The suite closes it.
type Factory func(t *testing.T) storage.Backend

// Run runs the full conformance suite against backends produced by newBackend.
func Run(t *testing.T, newBackend Factory) {
	t.Helper()
	_ = newBackend
}
