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
	"strings"
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
		{"Optional", optionalTests()},
	}
}

// Run runs the full conformance suite against backends produced by newBackend.
func Run(t *testing.T, newBackend Factory) {
	t.Helper()
	RunWithOptions(t, newBackend, Options{})
}

// Options adjust the suite for backends with documented limitations.
//
// Subtests that need versioning or object lock are skipped automatically
// when the backend answers storage.ErrNotSupported; Options are for the
// rest.
type Options struct {
	// Skip maps subtest names, relative to the suite and as printed by go
	// test (e.g. "Objects/ReaderSurvivesOverwrite/Unversioned"), to the
	// reason they are skipped. The subtest and everything under it is
	// skipped.
	Skip map[string]string
	// NoControlKeys is for backends that keep keys in a remote S3 service,
	// which refuses keys with characters XML cannot represent. Fixtures
	// that use control characters only for their sort order get printable
	// substitutes that keep it: every control character becomes '!'
	// (0x21), so "a\x00b" becomes "a!b", which still sorts between "a" and
	// "a-c". Subtests whose purpose is storing control-character keys
	// verbatim (Objects/Keys) are skipped.
	NoControlKeys bool
	// OpaqueVersionIDs is for backends that pass through version IDs they
	// do not generate (a remote S3 service's IDs). Version IDs must then
	// satisfy the S3 layer's rule (1-1024 printable, non-space ASCII
	// characters, never "null") instead of being at most 64 URL-safe
	// characters. Such IDs carry no position, so a WalkVersions whose
	// versionIDMarker names a deleted version may re-deliver the remaining
	// versions of keyMarker that are newer than the deleted one, as the
	// storage.Backend contract allows; skipping a version still fails.
	OpaqueVersionIDs bool
}

// suite is the configuration shared by all subtests of one run.
type suite struct {
	factory Factory
	root    string // name of the test that called RunWithOptions
	skip    map[string]string

	noControlKeys    bool // Options.NoControlKeys
	opaqueVersionIDs bool // Options.OpaqueVersionIDs
}

// skipListed skips t when Options.Skip lists it.
func (s *suite) skipListed(t *testing.T) {
	t.Helper()
	if reason, ok := s.skip[strings.TrimPrefix(t.Name(), s.root+"/")]; ok {
		t.Skip(reason)
	}
}

// RunWithOptions is Run with options.
func RunWithOptions(t *testing.T, newBackend Factory, opts Options) {
	t.Helper()
	if newBackend == nil {
		t.Fatal("storagetest.Run: nil Factory")
	}
	s := &suite{
		factory:          newBackend,
		root:             t.Name(),
		skip:             opts.Skip,
		noControlKeys:    opts.NoControlKeys,
		opaqueVersionIDs: opts.OpaqueVersionIDs,
	}
	for _, g := range groups() {
		t.Run(g.name, func(t *testing.T) {
			s.skipListed(t)
			for _, tc := range g.tests {
				t.Run(tc.name, func(t *testing.T) {
					s.skipListed(t)
					tc.fn(newEnv(t, s))
				})
			}
		})
	}
}
