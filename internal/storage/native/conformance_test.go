package native_test

import (
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
	"github.com/seanmmitchell/gos3/internal/storage/storagetest"
)

// nativeSkips are the conformance subtests native buckets cannot pass for
// fundamental reasons (see the package documentation).
var nativeSkips = map[string]string{
	// Walks report what the remote's listings return (key, size, ETag,
	// time, storage class, version): user metadata, tags, checksums etc.
	// would need a HEAD per listed object. HEAD/GET round trips of the
	// same metadata are covered by TestMetadataRoundTrip.
	"Objects/MetadataRoundTrip": "native listings carry only the remote listing's metadata",
	// Without versioning the replaced content is gone on the remote: the
	// reader fails with an error once it has to issue a new request.
	"Objects/ReaderSurvivesOverwrite/Unversioned": "a reader cannot re-read content replaced on the remote",
}

// TestConformance runs the storage conformance suite against native
// buckets on an in-process remote with versioning and object lock, so the
// versioning and object lock groups run too. Keys with control characters
// cannot be stored on an S3 service (NoControlKeys); version IDs are the
// remote's (OpaqueVersionIDs).
func TestConformance(t *testing.T) {
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend {
		return newHarness(t, s3clienttest.Options{}, nil)
	}, storagetest.Options{Skip: nativeSkips, NoControlKeys: true, OpaqueVersionIDs: true})
}

// TestConformanceSmallPages runs the walk-heavy groups with remote
// listings of 3 entries per page, so walks resume within and across keys
// all the time.
func TestConformanceSmallPages(t *testing.T) {
	skips := map[string]string{}
	for k, v := range nativeSkips {
		skips[k] = v
	}
	for _, g := range []string{"Buckets", "Objects", "UpdateObjectMeta", "Conditional", "Multipart", "Concurrency", "Optional"} {
		skips[g] = "covered by TestConformance"
	}
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend {
		h := newHarness(t, s3clienttest.Options{}, nil)
		native.SetListPage(h.st, 3)
		return h
	}, storagetest.Options{Skip: skips, NoControlKeys: true, OpaqueVersionIDs: true})
}
