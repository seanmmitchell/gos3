package gateway_test

import (
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/gateway"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/routed"
	"github.com/seanmmitchell/gos3/internal/storage/storagetest"
)

// newRouted returns a router whose default backing "gw" is a gateway
// store over a fresh in-process remote.
func newRouted(t *testing.T, shared bool) storage.Backend {
	r := s3clienttest.Start(t)
	opts := gateway.Options{Name: "gw", Client: r.Client(t), StateDir: t.TempDir()}
	if shared {
		r.CreateBucket(t, "shared")
		opts.Bucket, opts.Prefix = "shared", "pfx/"
	}
	st, err := gateway.Open(opts)
	if err != nil {
		t.Fatalf("gateway.Open: %v", err)
	}
	rb := routed.New(routed.Options{
		Primary: memory.New(),
		Stores:  map[string]routed.DataStore{"gw": st},
		Default: "gw",
	})
	if shared {
		return renamed{b: rb}
	}
	return renamed{b: rb, prefix: "gw-"}
}

// gatewaySkips are the conformance subtests a gateway cannot pass for
// fundamental reasons (see the package documentation).
var gatewaySkips = map[string]string{
	// Walks report what ListObjectsV2 returns (key, size, remote ETag,
	// time, storage class): user metadata, tags, checksums etc. would need
	// a HEAD per listed object. HEAD/GET round trips of the same metadata
	// are covered by TestMetadataRoundTrip.
	"Objects/MetadataRoundTrip": "gateway listings carry only ListObjectsV2 metadata",
	// Without remote versioning the replaced content is gone: the reader
	// fails with an error once it has to issue a new request (Seek).
	"Objects/ReaderSurvivesOverwrite/Unversioned": "a gateway reader cannot re-read content replaced on the remote",
	// S3 services refuse keys with characters XML cannot represent (they
	// could not be listed); the gateway rejects them with ErrInvalidName.
	"Objects/Keys":            "keys with control characters cannot be stored on an S3 service",
	"Walk/ObjectsOrder":       "uses a key with a NUL byte, which S3 services refuse",
	"Walk/ObjectsPrefixAfter": "uses a key with a NUL byte, which S3 services refuse",
}

func TestConformancePerBucket(t *testing.T) {
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend { return newRouted(t, false) },
		storagetest.Options{Skip: gatewaySkips})
}

func TestConformanceShared(t *testing.T) {
	// Objects/Keys is skipped in both modes; in shared mode its 1024-byte
	// keys would also exceed S3's limit once the prefix and bucket name are
	// added (rejected with storage.ErrInvalidName).
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend { return newRouted(t, true) },
		storagetest.Options{Skip: gatewaySkips})
}
