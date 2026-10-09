// Package storagetest is the conformance test suite every storage.Backend
// implementation must pass.
//
// Usage from a backend package:
//
//	func TestConformance(t *testing.T) {
//		storagetest.Run(t, func(t *testing.T) storage.Backend { return New() })
//	}
//
// The suite is backend-agnostic: it uses only the public storage API. The
// doc comments in internal/storage/storage.go are the specification; where
// they are silent the suite follows AWS S3 semantics and says so in a
// comment next to the assertion. Every subtest gets a fresh backend from the
// factory and closes it when it finishes, so factories should return an
// empty backend each time (e.g. rooted in t.TempDir()).
//
// Run's signature is a fixed contract (see docs/ARCHITECTURE.md); the suite
// is owned by the storage-conformance work item.
package storagetest

import (
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Factory returns a fresh, empty backend for one subtest. The suite closes it.
type Factory func(t *testing.T) storage.Backend

// testCase is one conformance subtest; fn receives a fresh backend.
type testCase struct {
	name string
	fn   func(e *env)
}

// group is a named set of subtests.
type group struct {
	name  string
	tests []testCase
}

func groups() []group {
	return []group{
		{"Buckets", bucketTests()},
		{"Objects", objectTests()},
		{"Versioning", versioningTests()},
		{"UpdateObjectMeta", updateMetaTests()},
		{"Conditional", conditionalTests()},
		{"Walk", walkTests()},
		{"List", listTests()},
		{"Multipart", multipartTests()},
		{"Concurrency", concurrencyTests()},
	}
}

// Run runs the full conformance suite against backends produced by newBackend.
func Run(t *testing.T, newBackend Factory) {
	t.Helper()
	if newBackend == nil {
		t.Fatal("storagetest.Run: nil Factory")
	}
	for _, g := range groups() {
		t.Run(g.name, func(t *testing.T) {
			for _, tc := range g.tests {
				t.Run(tc.name, func(t *testing.T) {
					tc.fn(newEnv(t, newBackend))
				})
			}
		})
	}
}
