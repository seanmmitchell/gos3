package s3account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/storagetest"
)

// The conformance suite runs against the four ways an s3 backing stores a
// bucket. Every subtest gets a fresh backing.

// conformant hides the output-only BucketInfo fields Kind and Location,
// which the suite (comparing whole BucketInfo values) does not expect.
// Embedding keeps every optional interface of the Backend.
type conformant struct{ *Backend }

func (c conformant) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	info, err := c.Backend.GetBucket(ctx, name)
	info.Kind, info.Location = "", ""
	return info, err
}

func (c conformant) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	list, err := c.Backend.ListBuckets(ctx)
	for i := range list {
		list[i].Kind, list[i].Location = "", ""
	}
	return list, err
}

// TestConformanceWrappedHome: wrapped buckets in the home (the shared
// container).
func TestConformanceWrappedHome(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	claims := NewClaims()
	var n atomic.Int64
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		return conformant{openReady(t, homeOptions(t, r, claims, "home", fmt.Sprintf("h%05d/", n.Add(1))))}
	})
}

// TestConformanceWrappedDedicated: wrapped buckets in a dedicated container
// (a remote bucket created for it) of a backing keeping its records
// locally.
func TestConformanceWrappedDedicated(t *testing.T) {
	r := s3clienttest.Start(t)
	claims := NewClaims()
	var n atomic.Int64
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		o := localOptions(t, r, claims, t.TempDir())
		o.DefaultKind = storage.BucketKindWrapped
		b := openReady(t, o)
		ctx := context.Background()
		ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: fmt.Sprintf("ded%05d", n.Add(1)), CreateBucket: true})
		noErr(t, "AddContainer", err)
		noErr(t, "SetDefaultContainer", b.SetDefaultContainer(ctx, ci.ID))
		return conformant{b}
	})
}

// TestConformanceWrappedExisting: wrapped buckets in a container at a
// prefix of an existing remote bucket that holds other data.
func TestConformanceWrappedExisting(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	r.CreateBucket(t, "shared")
	remotePut(t, r.Client(t), "shared", "other/data.txt", "not gos3's")
	claims := NewClaims()
	var n atomic.Int64
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		i := n.Add(1)
		b := openReady(t, homeOptions(t, r, claims, "home", fmt.Sprintf("h%05d/", i)))
		ctx := context.Background()
		ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "shared", Prefix: fmt.Sprintf("c%05d/", i)})
		noErr(t, "AddContainer", err)
		noErr(t, "SetDefaultContainer", b.SetDefaultContainer(ctx, ci.ID))
		return conformant{b}
	})
}

// nativeSkips are the subtests native buckets cannot pass (see the native
// package's conformance test).
var nativeSkips = map[string]string{
	"Objects/MetadataRoundTrip":                   "native listings carry only the remote listing's metadata",
	"Objects/ReaderSurvivesOverwrite/Unversioned": "a reader cannot re-read content replaced on the remote",
	// The backing implements these for its wrapped buckets; native buckets
	// report them unavailable through BucketCaps (Import, Compression).
	"Optional/VersionImporter":                        "native buckets cannot receive imported versions",
	"Optional/Compression/RoundTrip":                  "native buckets store data as-is",
	"Optional/Compression/Usage":                      "native buckets store data as-is",
	"Optional/Compression/UpdateMetaKeepsCompression": "native buckets store data as-is",
	"Optional/Compression/Multipart":                  "native buckets store data as-is",
	// The object lock configuration of a native bucket is mirrored to the
	// remote bucket, so it must be a valid ObjectLockConfiguration (the S3
	// layer validates it); the suite stores arbitrary documents.
	"Buckets/ConfigAllKeys": "a native bucket's object lock configuration must be a valid document",
}

// nativeSuite creates every bucket as a native bucket whose remote bucket
// has a valid S3 name (the suite's bucket names are not all valid).
type nativeSuite struct{ conformant }

func remoteNameOf(bucket string) string {
	sum := sha256.Sum256([]byte(bucket))
	return "nb-" + hex.EncodeToString(sum[:10])
}

func (s nativeSuite) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	opts.Target = &storage.BucketTarget{Kind: storage.BucketKindNative, RemoteBucket: remoteNameOf(name)}
	return s.conformant.CreateBucket(ctx, name, opts)
}

// TestConformanceNative: native buckets on a remote with versioning and
// object lock, so the versioning and object lock groups run.
func TestConformanceNative(t *testing.T) {
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend {
		r := s3clienttest.Start(t)
		o := localOptions(t, r, NewClaims(), t.TempDir())
		o.DefaultKind = storage.BucketKindNative
		return nativeSuite{conformant{openReady(t, o)}}
	}, storagetest.Options{Skip: nativeSkips, NoControlKeys: true, OpaqueVersionIDs: true})
}
