package routed_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/routed"
)

// fakeStore records the calls the router makes. Object methods other than
// GetObjectInfo and NewObjectWriter are not used by these tests (the
// embedded nil interface would panic).
type fakeStore struct {
	routed.DataStore

	mu        sync.Mutex
	attached  []storage.BucketInfo
	detached  []string
	infoCalls []string
	attachErr error
	notEmpty  bool
	closed    *[]string // appended "store" on Close
}

func (f *fakeStore) AttachBucket(_ context.Context, info storage.BucketInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attachErr != nil {
		return f.attachErr
	}
	f.attached = append(f.attached, info)
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

func (f *fakeStore) NewObjectWriter(_ context.Context, bucket, key string) (storage.ObjectWriter, error) {
	return &fakeWriter{bucket: bucket, key: key}, nil
}

func (f *fakeStore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.closed = append(*f.closed, "store")
	return nil
}

// fakeWriter commits nothing; like a DataStore, it passes no configuration
// documents to BeforePublish.
type fakeWriter struct{ bucket, key string }

func (*fakeWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*fakeWriter) Abort() error                { return nil }
func (w *fakeWriter) Commit(_ context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	if opts.BeforePublish != nil {
		if err := opts.BeforePublish(&meta, nil, time.Now(), func(string) []byte { return nil }); err != nil {
			return storage.ObjectInfo{}, err
		}
	}
	return storage.ObjectInfo{ObjectMeta: meta, Bucket: w.bucket, Key: w.key, VersionID: storage.NullVersionID}, nil
}

// primary is a memory backend that records Close.
type primary struct {
	*memory.Backend
	closed *[]string
}

func (p primary) Close() error {
	*p.closed = append(*p.closed, "primary")
	return p.Backend.Close()
}

type fixture struct {
	b      *routed.Backend
	prim   *memory.Backend
	fs     *fakeStore
	closed []string
}

func newRouter(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{prim: memory.New()}
	f.fs = &fakeStore{closed: &f.closed}
	f.b = routed.New(routed.Options{Primary: primary{Backend: f.prim, closed: &f.closed}, Store: f.fs})
	t.Cleanup(func() { _ = f.b.Close() })
	return f
}

func TestCreateBucketAttachesFirst(t *testing.T) {
	ctx := t.Context()
	f := newRouter(t)
	if err := f.b.CreateBucket(ctx, "b", storage.CreateBucketOptions{Region: "r1", Owner: "alice", Backing: "gw"}); err != nil {
		t.Fatal(err)
	}
	want := []storage.BucketInfo{{Name: "b", Region: "r1", Owner: "alice", Backing: "gw"}}
	if !slices.Equal(f.fs.attached, want) {
		t.Errorf("attached %+v, want %+v", f.fs.attached, want)
	}
	info, err := f.b.GetBucket(ctx, "b")
	if err != nil || info.Owner != "alice" {
		t.Errorf("GetBucket = %+v, %v", info, err)
	}
	if err := f.b.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); !errors.Is(err, storage.ErrBucketExists) {
		t.Errorf("second CreateBucket: %v, want ErrBucketExists", err)
	}
	if len(f.fs.attached) != 1 {
		t.Errorf("an existing bucket was attached again: %+v", f.fs.attached)
	}
}

func TestAttachFailureAbortsCreate(t *testing.T) {
	f := newRouter(t)
	f.fs.attachErr = storage.ErrBackingUnavailable
	if err := f.b.CreateBucket(t.Context(), "b", storage.CreateBucketOptions{}); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Errorf("CreateBucket with a failing attach: %v", err)
	}
	if _, err := f.b.GetBucket(t.Context(), "b"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("bucket recorded although attaching failed: %v", err)
	}
}

func TestVersioningAndObjectLock(t *testing.T) {
	ctx := t.Context()
	f := newRouter(t)
	err := f.b.CreateBucket(ctx, "locked", storage.CreateBucketOptions{ObjectLockEnabled: true})
	if !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("object lock bucket: %v, want ErrNotSupported", err)
	}
	if len(f.fs.attached) != 0 {
		t.Errorf("attached %+v for a refused bucket", f.fs.attached)
	}
	if err := f.b.CreateBucket(ctx, "gwb", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	// An existing name is reported as such, whatever the options.
	err = f.b.CreateBucket(ctx, "gwb", storage.CreateBucketOptions{ObjectLockEnabled: true})
	if !errors.Is(err, storage.ErrBucketExists) {
		t.Errorf("CreateBucket of an existing bucket with object lock: %v, want ErrBucketExists", err)
	}
	for _, st := range []storage.VersioningStatus{storage.VersioningEnabled, storage.VersioningSuspended} {
		if err := f.b.SetBucketVersioning(ctx, "gwb", st); !errors.Is(err, storage.ErrNotSupported) {
			t.Errorf("versioning %v: %v", st, err)
		}
	}
	if err := f.b.SetBucketObjectLock(ctx, "gwb", true); !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("enabling object lock: %v", err)
	}
	if err := f.b.SetBucketObjectLock(ctx, "gwb", false); err != nil {
		t.Errorf("disabling object lock: %v", err)
	}
	if info, _ := f.b.GetBucket(ctx, "gwb"); info.Versioning != storage.VersioningUnversioned || info.ObjectLockEnabled {
		t.Errorf("bucket changed: %+v", info)
	}
	if err := f.b.SetBucketVersioning(ctx, "missing", storage.VersioningEnabled); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("versioning on a missing bucket: %v", err)
	}
}

func TestDeleteBucket(t *testing.T) {
	ctx := t.Context()
	f := newRouter(t)
	if err := f.b.CreateBucket(ctx, "gwb", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.b.PutBucketConfig(ctx, "gwb", storage.ConfigPolicy, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	// Cache the bucket's existence.
	if _, err := f.b.GetObjectInfo(ctx, "gwb", "k", ""); err != nil {
		t.Fatal(err)
	}
	f.fs.notEmpty = true
	if err := f.b.DeleteBucket(ctx, "gwb"); !errors.Is(err, storage.ErrBucketNotEmpty) {
		t.Errorf("DeleteBucket of a non-empty bucket: %v", err)
	}
	if _, err := f.b.GetBucket(ctx, "gwb"); err != nil || len(f.fs.detached) != 0 {
		t.Errorf("refused delete: GetBucket %v, detached %q", err, f.fs.detached)
	}
	f.fs.notEmpty = false
	if err := f.b.DeleteBucket(ctx, "gwb"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if !slices.Equal(f.fs.detached, []string{"gwb"}) {
		t.Errorf("detached %q, want [gwb]", f.fs.detached)
	}
	if _, err := f.b.GetBucket(ctx, "gwb"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("GetBucket after delete: %v", err)
	}
	if _, err := f.b.GetBucketConfig(ctx, "gwb", storage.ConfigPolicy); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("config after delete: %v", err)
	}
	// Object operations on the deleted bucket no longer reach the store
	// (the existence cache was invalidated).
	if _, err := f.b.GetObjectInfo(ctx, "gwb", "k", ""); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("GetObjectInfo after delete: %v", err)
	}
	if len(f.fs.infoCalls) != 1 {
		t.Errorf("store calls %q", f.fs.infoCalls)
	}
	if err := f.b.DeleteBucket(ctx, "gwb"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestObjectOperationsGoToStore(t *testing.T) {
	ctx := t.Context()
	f := newRouter(t)
	if err := f.b.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.GetObjectInfo(ctx, "b", "k", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.GetObjectInfo(ctx, "missing", "k", ""); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("GetObjectInfo in a missing bucket: %v", err)
	}
	if _, err := f.b.NewObjectWriter(ctx, "missing", "k"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("NewObjectWriter in a missing bucket: %v", err)
	}
	if !slices.Equal(f.fs.infoCalls, []string{"b/k"}) {
		t.Errorf("store calls %q", f.fs.infoCalls)
	}
	// Nothing reached the primary's object space.
	if _, err := f.prim.GetObjectInfo(ctx, "b", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("primary object: %v", err)
	}
}

func TestBeforePublishSeesPrimaryConfigs(t *testing.T) {
	ctx := t.Context()
	f := newRouter(t)
	if err := f.b.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.b.PutBucketConfig(ctx, "b", storage.ConfigPolicy, []byte("doc")); err != nil {
		t.Fatal(err)
	}
	w, err := f.b.NewObjectWriter(ctx, "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	var seen []byte
	_, err = w.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{
		BeforePublish: func(_, _ *storage.ObjectMeta, _ time.Time, config func(string) []byte) error {
			seen = config(storage.ConfigPolicy)
			if config("absent") != nil {
				t.Error("absent config is not nil")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(seen) != "doc" {
		t.Errorf("BeforePublish saw %q, want the primary's document", seen)
	}
}

func TestOptionalInterfaces(t *testing.T) {
	ctx := t.Context()
	f := newRouter(t)
	if err := f.b.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := any(f.b).(storage.VersionImporter); ok {
		t.Error("routed.Backend implements storage.VersionImporter")
	}
	providers, err := f.b.EncryptionProviders(ctx, "b")
	if err != nil || providers == nil || len(providers) != 0 {
		t.Errorf("EncryptionProviders = %q, %v; want an empty list", providers, err)
	}
	if _, err := f.b.EncryptionProviders(ctx, "missing"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("EncryptionProviders of a missing bucket: %v", err)
	}
	for _, mode := range []string{storage.EncryptionAES256, storage.EncryptionRestic} {
		if err := f.b.PrepareEncryption(ctx, "b", mode); !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("PrepareEncryption(%s): %v, want ErrUnsupported", mode, err)
		}
		if err := f.b.ValidateEncryptionSettings(ctx, "b", storage.EncryptionSettings{Provider: mode}); !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("ValidateEncryptionSettings(%s): %v, want ErrUnsupported", mode, err)
		}
	}
	if err := f.b.PrepareEncryption(ctx, "missing", storage.EncryptionAES256); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("PrepareEncryption of a missing bucket: %v", err)
	}
	if err := f.b.PutBucketConfig(ctx, "b", storage.ConfigPolicy, []byte("doc")); err != nil {
		t.Fatal(err)
	}
	var viewed string
	if err := f.b.ViewBucketConfig(ctx, "b", storage.ConfigPolicy, func(d []byte) error { viewed = string(d); return nil }); err != nil || viewed != "doc" {
		t.Errorf("ViewBucketConfig = %q, %v", viewed, err)
	}
	// Without a ConfigViewer primary the document is copied.
	b2 := routed.New(routed.Options{Primary: noViewer{f.prim}, Store: &fakeStore{closed: new([]string)}})
	viewed = ""
	if err := b2.ViewBucketConfig(ctx, "b", storage.ConfigPolicy, func(d []byte) error { viewed = string(d); return nil }); err != nil || viewed != "doc" {
		t.Errorf("ViewBucketConfig without a viewer = %q, %v", viewed, err)
	}
	if err := b2.ViewBucketConfig(ctx, "b", "absent", func([]byte) error { return nil }); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Errorf("ViewBucketConfig of an absent document: %v", err)
	}
}

// noViewer hides the memory backend's ConfigViewer implementation.
type noViewer struct{ storage.Backend }

func TestCloseOrder(t *testing.T) {
	f := newRouter(t)
	if err := f.b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.b.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"store", "primary"}; !slices.Equal(f.closed, want) {
		t.Errorf("closed %q, want %q", f.closed, want)
	}
}
