package native_test

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
)

// harness is a minimal storage.Backend over a native.Store, for the
// conformance suite and the unit tests: it keeps the bucket records and
// configuration documents in memory (the s3 account backing's job in the
// server) and maps gos3 bucket b to the remote bucket "nb-"+b (the suite's
// names are not all valid S3 bucket names).
type harness struct {
	t      testing.TB
	remote *s3clienttest.Remote
	c      *s3client.Client // plain client of the remote
	opts   native.Options
	st     *native.Store

	admin sync.Mutex // serializes bucket operations

	mu      sync.RWMutex
	buckets map[string]*hbucket
	drifts  []string
}

type hbucket struct {
	info    storage.BucketInfo
	bind    native.Binding
	configs map[string][]byte
}

var (
	_ storage.Backend                = (*harness)(nil)
	_ storage.ConditionalDeleter     = (*harness)(nil)
	_ storage.NewerVersionsDeleter   = (*harness)(nil)
	_ storage.CheckedDeleter         = (*harness)(nil)
	_ storage.ConditionalMetaUpdater = (*harness)(nil)
	_ storage.UploadWalker           = (*harness)(nil)
)

// remoteName is the remote bucket of gos3 bucket b.
func remoteName(b string) string { return "nb-" + b }

// newHarness starts a remote (configured by ro) and opens a store on it;
// mod adjusts the store's options.
func newHarness(t testing.TB, ro s3clienttest.Options, mod func(*native.Options)) *harness {
	t.Helper()
	r := s3clienttest.StartWith(t, ro)
	h := &harness{t: t, remote: r, c: r.Client(t), buckets: map[string]*hbucket{}}
	h.opts = native.Options{
		Name:    "nat",
		Client:  h.c,
		State:   native.MemoryState(),
		Resolve: h.resolve,
		OnDrift: func(bucket, reason string) {
			h.mu.Lock()
			h.drifts = append(h.drifts, bucket+": "+reason)
			h.mu.Unlock()
		},
	}
	if mod != nil {
		mod(&h.opts)
	}
	h.open()
	t.Cleanup(func() { _ = h.st.Close() })
	return h
}

// open (re)opens the store with h.opts, closing the previous one.
func (h *harness) open() {
	h.t.Helper()
	if h.st != nil {
		_ = h.st.Close()
	}
	st, err := native.Open(h.opts)
	if err != nil {
		h.t.Fatalf("native.Open: %v", err)
	}
	h.st = st
}

func (h *harness) resolve(_ context.Context, bucket string) (native.Bucket, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	b := h.buckets[bucket]
	if b == nil {
		return native.Bucket{}, fmt.Errorf("%w: %q", storage.ErrBucketNotFound, bucket)
	}
	return native.Bucket{Binding: b.bind, Versioning: b.info.Versioning, ObjectLock: b.info.ObjectLockEnabled}, nil
}

// driftLog returns the drift reports so far.
func (h *harness) driftLog() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return slices.Clone(h.drifts)
}

// record returns bucket's record (nil if none) under the lock.
func (h *harness) record(name string) *hbucket {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.buckets[name]
}

// adopt binds gos3 bucket name to an existing remote bucket.
func (h *harness) adopt(ctx context.Context, name, remote, region string) (native.RemoteState, error) {
	h.admin.Lock()
	defer h.admin.Unlock()
	st, err := h.st.AttachBucket(ctx, native.AttachRequest{Bucket: name, Remote: remote, Region: region, Intent: native.Adopt})
	if err != nil {
		return st, err
	}
	h.add(name, storage.CreateBucketOptions{}, st)
	return st, nil
}

func (h *harness) add(name string, opts storage.CreateBucketOptions, st native.RemoteState) {
	b := &hbucket{
		info: storage.BucketInfo{
			Name: name, CreatedAt: time.Now().UTC(), Region: opts.Region, Owner: opts.Owner,
			Versioning: st.Versioning, ObjectLockEnabled: st.ObjectLock,
		},
		bind:    st.Binding,
		configs: map[string][]byte{},
	}
	if st.LockConfig != nil {
		b.configs[storage.ConfigObjectLock] = st.LockConfig
	}
	h.mu.Lock()
	h.buckets[name] = b
	h.mu.Unlock()
}

// --- buckets ---

func (h *harness) ListBuckets(context.Context) ([]storage.BucketInfo, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]storage.BucketInfo, 0, len(h.buckets))
	for _, b := range h.buckets {
		out = append(out, b.info)
	}
	slices.SortFunc(out, func(a, b storage.BucketInfo) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (h *harness) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	h.admin.Lock()
	defer h.admin.Unlock()
	if h.record(name) != nil {
		return fmt.Errorf("%w: %q", storage.ErrBucketExists, name)
	}
	st, err := h.st.AttachBucket(ctx, native.AttachRequest{Bucket: name, Remote: remoteName(name), Intent: native.Create, ObjectLock: opts.ObjectLockEnabled})
	if err != nil {
		return err
	}
	h.add(name, opts, st)
	return nil
}

func (h *harness) GetBucket(_ context.Context, name string) (storage.BucketInfo, error) {
	b := h.record(name)
	if b == nil {
		return storage.BucketInfo{}, fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return b.info, nil
}

func (h *harness) DeleteBucket(ctx context.Context, name string) error {
	h.admin.Lock()
	defer h.admin.Unlock()
	b := h.record(name)
	if b == nil {
		return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	}
	if err := h.st.CheckEmpty(ctx, name); err != nil {
		return err
	}
	if err := h.st.DetachBucket(ctx, name, b.bind.Owned); err != nil {
		return err
	}
	h.mu.Lock()
	delete(h.buckets, name)
	h.mu.Unlock()
	return nil
}

func (h *harness) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	h.admin.Lock()
	defer h.admin.Unlock()
	if h.record(name) == nil {
		return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	}
	if err := h.st.SetVersioning(ctx, name, status); err != nil {
		return err
	}
	h.mu.Lock()
	h.buckets[name].info.Versioning = status
	h.mu.Unlock()
	return nil
}

func (h *harness) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	h.admin.Lock()
	defer h.admin.Unlock()
	if h.record(name) == nil {
		return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	}
	if !enabled {
		return fmt.Errorf("%w: object lock cannot be disabled", storage.ErrNotSupported)
	}
	if err := h.st.EnableObjectLock(ctx, name); err != nil {
		return err
	}
	h.mu.Lock()
	h.buckets[name].info.ObjectLockEnabled = true
	h.buckets[name].info.Versioning = storage.VersioningEnabled
	h.mu.Unlock()
	return nil
}

func (h *harness) GetBucketConfig(_ context.Context, bucket, key string) ([]byte, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	b := h.buckets[bucket]
	if b == nil {
		return nil, fmt.Errorf("%w: %q", storage.ErrBucketNotFound, bucket)
	}
	d, ok := b.configs[key]
	if !ok {
		return nil, storage.ErrConfigNotFound
	}
	return slices.Clone(d), nil
}

func (h *harness) PutBucketConfig(_ context.Context, bucket, key string, data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.buckets[bucket]
	if b == nil {
		return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, bucket)
	}
	b.configs[key] = slices.Clone(data)
	return nil
}

func (h *harness) DeleteBucketConfig(_ context.Context, bucket, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.buckets[bucket]
	if b == nil {
		return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, bucket)
	}
	delete(b.configs, key)
	return nil
}

// withConfigs makes opts.BeforePublish see the bucket's configuration
// documents (as the s3 account backing does).
func (h *harness) withConfigs(bucket string, opts storage.CommitOptions) storage.CommitOptions {
	hook := opts.BeforePublish
	if hook == nil {
		return opts
	}
	h.mu.RLock()
	var configs map[string][]byte
	if b := h.buckets[bucket]; b != nil {
		configs = maps.Clone(b.configs)
	}
	h.mu.RUnlock()
	opts.BeforePublish = func(meta, current *storage.ObjectMeta, created time.Time, _ func(string) []byte) error {
		return hook(meta, current, created, func(key string) []byte { return configs[key] })
	}
	return opts
}

// --- objects ---

type harnessWriter struct {
	storage.ObjectWriter
	h      *harness
	bucket string
}

func (w harnessWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	return w.ObjectWriter.Commit(ctx, meta, w.h.withConfigs(w.bucket, opts))
}

func (h *harness) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	w, err := h.st.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	return harnessWriter{w, h, bucket}, nil
}

func (h *harness) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	return h.st.GetObjectInfo(ctx, bucket, key, versionID)
}

func (h *harness) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	return h.st.OpenObject(ctx, bucket, key, versionID)
}

func (h *harness) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	return h.st.UpdateObjectMeta(ctx, bucket, key, versionID, fn)
}

func (h *harness) UpdateObjectMetaIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	return h.st.UpdateObjectMetaIf(ctx, bucket, key, versionID, check, fn)
}

func (h *harness) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	return h.st.DeleteObject(ctx, bucket, key, versionID)
}

func (h *harness) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	return h.st.DeleteObjectIf(ctx, bucket, key, versionID, check)
}

func (h *harness) DeleteVersionIf(ctx context.Context, bucket, key, versionID string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, error) {
	return h.st.DeleteVersionIf(ctx, bucket, key, versionID, limit, check)
}

func (h *harness) DeleteObjectChecked(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo, bool) error) (storage.DeleteResult, error) {
	return h.st.DeleteObjectChecked(ctx, bucket, key, versionID, check)
}

func (h *harness) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	return h.st.WalkObjects(ctx, bucket, prefix, after, fn)
}

func (h *harness) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	return h.st.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn)
}

// --- multipart ---

func (h *harness) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	return h.st.CreateMultipartUpload(ctx, bucket, key, meta, initiator)
}

func (h *harness) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	return h.st.GetMultipartUpload(ctx, bucket, key, uploadID)
}

func (h *harness) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	return h.st.NewPartWriter(ctx, bucket, key, uploadID, partNumber)
}

func (h *harness) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	return h.st.ListParts(ctx, bucket, key, uploadID)
}

func (h *harness) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	return h.st.ListMultipartUploads(ctx, bucket)
}

func (h *harness) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	return h.st.WalkMultipartUploads(ctx, bucket, prefix, after, fn)
}

func (h *harness) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	return h.st.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, h.withConfigs(bucket, opts))
}

func (h *harness) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	return h.st.AbortMultipartUpload(ctx, bucket, key, uploadID)
}

func (h *harness) Close() error { return h.st.Close() }
