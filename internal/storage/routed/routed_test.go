package routed_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/routed"
)

// fakeStore records the calls the router makes. Object methods other than
// GetObjectInfo are not used by these tests (the embedded nil interface
// would panic).
type fakeStore struct {
	routed.DataStore

	mu        sync.Mutex
	attached  []string
	detached  []string
	infoCalls []string
	attachErr error
	notEmpty  bool
	closed    int
}

func (f *fakeStore) AttachBucket(_ context.Context, info storage.BucketInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attachErr != nil {
		return f.attachErr
	}
	f.attached = append(f.attached, info.Name)
	return nil
}

func (f *fakeStore) CheckEmpty(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notEmpty {
		return storage.ErrBucketNotEmpty
	}
	return nil
}

func (f *fakeStore) DetachBucket(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detached = append(f.detached, bucket)
	return nil
}

func (f *fakeStore) GetObjectInfo(_ context.Context, bucket, key, _ string) (storage.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCalls = append(f.infoCalls, bucket+"/"+key)
	return storage.ObjectInfo{Bucket: bucket, Key: key, VersionID: storage.NullVersionID}, nil
}

func (f *fakeStore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func newRouter(t *testing.T, def string) (*routed.Backend, *memory.Backend, *fakeStore) {
	t.Helper()
	prim := memory.New()
	fs := &fakeStore{}
	b := routed.New(routed.Options{
		Primary:         prim,
		Stores:          map[string]routed.DataStore{"gw": fs},
		PrimaryBackings: []string{"blob"},
		Default:         def,
	})
	t.Cleanup(func() { _ = b.Close() })
	return b, prim, fs
}

func put(t *testing.T, b storage.Backend, bucket, key string) {
	t.Helper()
	w, err := b.NewObjectWriter(t.Context(), bucket, key)
	if err != nil {
		t.Fatalf("NewObjectWriter(%s/%s): %v", bucket, key, err)
	}
	if _, err := w.Commit(t.Context(), storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func backingOf(t *testing.T, b storage.Backend, bucket string) string {
	t.Helper()
	info, err := b.GetBucket(t.Context(), bucket)
	if err != nil {
		t.Fatalf("GetBucket(%s): %v", bucket, err)
	}
	return info.Backing
}

func TestDefaultBackingAndLocalAlias(t *testing.T) {
	ctx := t.Context()
	b, prim, fs := newRouter(t, "gw")
	for name, backing := range map[string]string{"dflt": "", "explicit": "gw", "loc": routed.BackingLocal, "blb": "blob"} {
		if err := b.CreateBucket(ctx, name, storage.CreateBucketOptions{Backing: backing}); err != nil {
			t.Fatalf("CreateBucket(%s, %q): %v", name, backing, err)
		}
	}
	for name, want := range map[string]string{"dflt": "gw", "explicit": "gw", "loc": "", "blb": "blob"} {
		if got := backingOf(t, b, name); got != want {
			t.Errorf("backing of %s = %q, want %q", name, got, want)
		}
	}
	if got := slices.Sorted(slices.Values(fs.attached)); !slices.Equal(got, []string{"dflt", "explicit"}) {
		t.Errorf("attached %q, want the two gateway buckets", got)
	}
	// Object operations go to the bucket's backing.
	if _, err := b.GetObjectInfo(ctx, "dflt", "k", ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fs.infoCalls, []string{"dflt/k"}) {
		t.Errorf("store calls %q", fs.infoCalls)
	}
	put(t, b, "loc", "k")
	put(t, b, "blb", "k")
	for _, bucket := range []string{"loc", "blb"} {
		if _, err := prim.GetObjectInfo(ctx, bucket, "k", ""); err != nil {
			t.Errorf("object of %s not in the primary: %v", bucket, err)
		}
	}
	if len(fs.infoCalls) != 1 {
		t.Errorf("primary buckets reached the store: %q", fs.infoCalls)
	}

	// With the server's own storage as the default, "" stays local.
	b2, _, fs2 := newRouter(t, routed.BackingLocal)
	if err := b2.CreateBucket(ctx, "x", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := backingOf(t, b2, "x"); got != "" || len(fs2.attached) != 0 {
		t.Errorf("default local: backing %q, attached %q", got, fs2.attached)
	}
}

func TestInvalidBacking(t *testing.T) {
	b, _, _ := newRouter(t, "")
	err := b.CreateBucket(t.Context(), "b", storage.CreateBucketOptions{Backing: "nope"})
	if !errors.Is(err, storage.ErrInvalidBacking) {
		t.Errorf("unknown backing: %v, want ErrInvalidBacking", err)
	}
	if _, err := b.GetBucket(t.Context(), "b"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("bucket created despite an invalid backing: %v", err)
	}
	// A default naming an unknown backing fails the same way.
	b2, _, _ := newRouter(t, "missing")
	if err := b2.CreateBucket(t.Context(), "b", storage.CreateBucketOptions{}); !errors.Is(err, storage.ErrInvalidBacking) {
		t.Errorf("unknown default backing: %v", err)
	}
}

func TestAttachFailureAbortsCreate(t *testing.T) {
	b, _, fs := newRouter(t, "gw")
	fs.attachErr = storage.ErrBackingUnavailable
	if err := b.CreateBucket(t.Context(), "b", storage.CreateBucketOptions{}); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("CreateBucket with a failing attach: %v", err)
	}
	if _, err := b.GetBucket(t.Context(), "b"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("bucket recorded although attaching failed: %v", err)
	}
}

func TestGatewayVersioningAndObjectLock(t *testing.T) {
	ctx := t.Context()
	b, _, fs := newRouter(t, "gw")
	err := b.CreateBucket(ctx, "locked", storage.CreateBucketOptions{ObjectLockEnabled: true})
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("object lock bucket on a gateway: %v, want ErrNotSupported", err)
	}
	if len(fs.attached) != 0 {
		t.Errorf("attached %q for a refused bucket", fs.attached)
	}
	if err := b.CreateBucket(ctx, "gwb", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	// An existing name is reported as such, whatever the options.
	err = b.CreateBucket(ctx, "gwb", storage.CreateBucketOptions{ObjectLockEnabled: true})
	if !errors.Is(err, storage.ErrBucketExists) {
		t.Errorf("CreateBucket of an existing gateway bucket with object lock: %v, want ErrBucketExists", err)
	}
	if err := b.SetBucketVersioning(ctx, "gwb", storage.VersioningEnabled); !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("versioning on a gateway bucket: %v", err)
	}
	if err := b.SetBucketObjectLock(ctx, "gwb", true); !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("object lock on a gateway bucket: %v", err)
	}
	if info, _ := b.GetBucket(ctx, "gwb"); info.Versioning != storage.VersioningUnversioned || info.ObjectLockEnabled {
		t.Errorf("gateway bucket changed: %+v", info)
	}
	// Local buckets keep every feature.
	if err := b.CreateBucket(ctx, "loc", storage.CreateBucketOptions{Backing: routed.BackingLocal, ObjectLockEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBucketVersioning(ctx, "loc", storage.VersioningSuspended); err != nil {
		t.Errorf("versioning on a local bucket: %v", err)
	}
	if err := b.SetBucketVersioning(ctx, "missing", storage.VersioningEnabled); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("versioning on a missing bucket: %v", err)
	}
}

func TestDeleteBucket(t *testing.T) {
	ctx := t.Context()
	b, _, fs := newRouter(t, "gw")
	if err := b.CreateBucket(ctx, "gwb", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.PutBucketConfig(ctx, "gwb", storage.ConfigPolicy, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	fs.notEmpty = true
	if err := b.DeleteBucket(ctx, "gwb"); !errors.Is(err, storage.ErrBucketNotEmpty) {
		t.Errorf("DeleteBucket of a non-empty store bucket: %v", err)
	}
	if _, err := b.GetBucket(ctx, "gwb"); err != nil || len(fs.detached) != 0 {
		t.Errorf("refused delete: GetBucket %v, detached %q", err, fs.detached)
	}
	fs.notEmpty = false
	if err := b.DeleteBucket(ctx, "gwb"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if !slices.Equal(fs.detached, []string{"gwb"}) {
		t.Errorf("detached %q, want [gwb]", fs.detached)
	}
	if _, err := b.GetBucket(ctx, "gwb"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("GetBucket after delete: %v", err)
	}
	if _, err := b.GetBucketConfig(ctx, "gwb", storage.ConfigPolicy); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("config after delete: %v", err)
	}
	// Object operations on the deleted bucket no longer reach the store.
	if _, err := b.GetObjectInfo(ctx, "gwb", "k", ""); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("GetObjectInfo after delete: %v", err)
	}
	if err := b.DeleteBucket(ctx, "gwb"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestUnconfiguredBacking(t *testing.T) {
	ctx := t.Context()
	prim := memory.New()
	// Buckets recorded while backing "old" existed.
	for _, name := range []string{"orphan", "orphan2"} {
		if err := prim.CreateBucket(ctx, name, storage.CreateBucketOptions{Backing: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := prim.CreateBucket(ctx, "fine", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	b := routed.New(routed.Options{Primary: prim})
	defer b.Close()
	if _, err := b.GetObjectInfo(ctx, "orphan", "k", ""); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("GetObjectInfo on an unconfigured backing: %v", err)
	}
	if _, err := b.NewObjectWriter(ctx, "orphan", "k"); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("NewObjectWriter on an unconfigured backing: %v", err)
	}
	if err := b.WalkObjects(ctx, "orphan", "", "", func(storage.ObjectInfo) error { return nil }); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("WalkObjects on an unconfigured backing: %v", err)
	}
	if _, err := b.ListMultipartUploads(ctx, "orphan"); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("ListMultipartUploads on an unconfigured backing: %v", err)
	}
	if err := b.SetBucketVersioning(ctx, "orphan", storage.VersioningEnabled); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("SetBucketVersioning on an unconfigured backing: %v", err)
	}
	got, err := b.UnavailableBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"orphan": "old", "orphan2": "old"}; !maps.Equal(got, want) {
		t.Errorf("UnavailableBuckets = %v, want %v", got, want)
	}
	// Such buckets can still be deleted (they hold nothing locally).
	if err := b.DeleteBucket(ctx, "orphan"); err != nil {
		t.Errorf("DeleteBucket of an unavailable bucket: %v", err)
	}
	if got, _ := b.UnavailableBuckets(ctx); len(got) != 1 {
		t.Errorf("UnavailableBuckets after delete = %v", got)
	}
}

func TestCloseClosesStoresOnce(t *testing.T) {
	b, _, fs := newRouter(t, "")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if fs.closed != 1 {
		t.Errorf("store closed %d times, want 1", fs.closed)
	}
}
