package storagetest_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/storagetest"
)

// remoteLike wraps a memory backend to behave like a backend that keeps
// its objects in a remote S3 service: keys with control characters are
// refused, and version IDs carry no position, so a WalkVersions from a
// versionIDMarker that is no longer a version of keyMarker re-delivers
// every remaining version of keyMarker. Like a backing with bucket kinds,
// it reports the output-only BucketInfo.Kind and Location. Embedding the
// interface hides the memory backend's optional interfaces (their subtests
// skip).
type remoteLike struct{ storage.Backend }

func withKind(bi storage.BucketInfo) storage.BucketInfo {
	bi.Kind, bi.Location = storage.BucketKindNative, "s3://remote-"+bi.Name
	return bi
}

func (b remoteLike) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	bi, err := b.Backend.GetBucket(ctx, name)
	if err != nil {
		return bi, err
	}
	return withKind(bi), nil
}

func (b remoteLike) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	list, err := b.Backend.ListBuckets(ctx)
	for i := range list {
		list[i] = withKind(list[i])
	}
	return list, err
}

func hasControl(key string) bool {
	return strings.ContainsFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func (b remoteLike) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	if hasControl(key) {
		return nil, fmt.Errorf("%w: control character in key %q", storage.ErrInvalidName, key)
	}
	return b.Backend.NewObjectWriter(ctx, bucket, key)
}

func (b remoteLike) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	if hasControl(key) {
		return storage.UploadInfo{}, fmt.Errorf("%w: control character in key %q", storage.ErrInvalidName, key)
	}
	return b.Backend.CreateMultipartUpload(ctx, bucket, key, meta, initiator)
}

func (b remoteLike) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	versions, err := markerVersions(ctx, b.Backend, bucket, prefix, keyMarker, versionIDMarker)
	if err != nil {
		return err
	}
	if versions == nil {
		return b.Backend.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn)
	}
	for _, o := range versions {
		if err := fn(o); err != nil {
			if err == storage.ErrStopWalk {
				return nil
			}
			return err
		}
	}
	return b.Backend.WalkVersions(ctx, bucket, prefix, keyMarker, "", fn)
}

// markerVersions returns the versions of keyMarker (newest first) when a
// walk from keyMarker/versionIDMarker starts within keyMarker and
// versionIDMarker is not one of them (non-nil, possibly empty); otherwise
// versions is nil.
func markerVersions(ctx context.Context, b storage.Backend, bucket, prefix, keyMarker, versionIDMarker string) (versions []storage.ObjectInfo, err error) {
	if keyMarker == "" || versionIDMarker == "" || !strings.HasPrefix(keyMarker, prefix) {
		return nil, nil
	}
	err = b.WalkVersions(ctx, bucket, keyMarker, "", "", func(o storage.ObjectInfo) error {
		if o.Key != keyMarker {
			return storage.ErrStopWalk
		}
		versions = append(versions, o)
		return nil
	})
	if err != nil || slices.ContainsFunc(versions, func(o storage.ObjectInfo) bool { return o.VersionID == versionIDMarker }) {
		return nil, err
	}
	if versions == nil {
		versions = []storage.ObjectInfo{}
	}
	return versions, nil
}

// TestRemoteLikeOptions: with NoControlKeys and OpaqueVersionIDs the whole
// suite passes against a backend that refuses control-character keys and
// re-delivers versions after a deleted version-ID marker.
func TestRemoteLikeOptions(t *testing.T) {
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend { return remoteLike{memory.New()} },
		storagetest.Options{NoControlKeys: true, OpaqueVersionIDs: true})
}

// skipping wraps a memory backend whose WalkVersions skips the remaining
// versions of keyMarker when versionIDMarker is not one of them, which no
// option allows.
type skipping struct{ storage.Backend }

func (b skipping) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	versions, err := markerVersions(ctx, b.Backend, bucket, prefix, keyMarker, versionIDMarker)
	if err != nil {
		return err
	}
	if versions != nil {
		versionIDMarker = "" // start after every version of keyMarker
	}
	return b.Backend.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn)
}

// TestOpaqueVersionIDsCatchSkips: OpaqueVersionIDs still fails a backend
// that skips versions after a deleted version-ID marker. The suite runs
// against it in a child process, which must fail in
// Walk/VersionsDeletedMarker.
func TestOpaqueVersionIDsCatchSkips(t *testing.T) {
	const childEnv = "STORAGETEST_SKIPPING_CHILD"
	if os.Getenv(childEnv) == "1" {
		storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend { return skipping{memory.New()} },
			storagetest.Options{OpaqueVersionIDs: true})
		return
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0],
		"-test.run=^"+t.Name()+"$/^Walk$/^VersionsDeletedMarker$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the suite passed a backend that skips versions after a deleted marker:\n%s", out)
	}
	if !bytes.Contains(out, []byte("--- FAIL: "+t.Name()+"/Walk/VersionsDeletedMarker")) ||
		!bytes.Contains(out, []byte("WalkVersions(keyMarker")) {
		t.Fatalf("child failed, but not in Walk/VersionsDeletedMarker's walk checks (%v):\n%s", err, out)
	}
}
