// Package routed implements the storage.Backend that places each bucket on
// its backing (see docs/backings.md).
//
// The primary backend (fs or memory) keeps every bucket record and every
// bucket configuration document. A bucket's BucketInfo.Backing, fixed at
// creation, decides where its objects and multipart uploads live:
//
//   - "" (the server's own storage) or a name the primary handles itself
//     (the fs backend's blob backings): the primary.
//   - the name of a DataStore (a gateway backing): that store.
//
// Object operations on a bucket whose backing is no longer configured fail
// with storage.ErrBackingUnavailable. Gateway buckets cannot be versioned
// or use object lock (storage.ErrNotSupported).
package routed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// BackingLocal is the name accepted for the server's own storage when
// creating a bucket; it is stored as "".
const BackingLocal = "local"

// Options configures New.
type Options struct {
	// Primary stores bucket records and configuration documents, and the
	// data of buckets that are not on a DataStore. Required.
	Primary storage.Backend
	// Stores are the external data stores (gateway backings) by name.
	Stores map[string]DataStore
	// PrimaryBackings are backing names the primary stores itself (the fs
	// backend's blob backings).
	PrimaryBackings []string
	// Default is the backing of buckets created without one ("" = the
	// server's own storage).
	Default string
	// Logger receives non-fatal errors. nil discards logs.
	Logger *slog.Logger
}

// Backend routes storage operations by bucket backing. Create it with New.
type Backend struct {
	primary  storage.Backend
	stores   map[string]DataStore
	primBack map[string]bool
	def      string
	log      *slog.Logger

	mu    sync.RWMutex
	cache map[string]string // bucket name -> backing
	// epoch counts cache invalidations; a lookup only caches its result
	// when no invalidation happened since it started, so a lookup racing
	// a DeleteBucket cannot re-insert the deleted bucket.
	epoch uint64

	closeOnce sync.Once
	closeErr  error
}

var (
	_ storage.Backend            = (*Backend)(nil)
	_ storage.ConditionalDeleter = (*Backend)(nil)
	_ storage.UploadWalker       = (*Backend)(nil)
)

// New returns a routing backend. It takes ownership of the primary and the
// stores: Close closes them.
func New(opts Options) *Backend {
	if opts.Primary == nil {
		panic("routed: Options.Primary is required")
	}
	b := &Backend{
		primary:  opts.Primary,
		stores:   make(map[string]DataStore, len(opts.Stores)),
		primBack: make(map[string]bool, len(opts.PrimaryBackings)),
		def:      opts.Default,
		log:      opts.Logger,
		cache:    make(map[string]string),
	}
	if b.def == BackingLocal {
		b.def = ""
	}
	for name, s := range opts.Stores {
		b.stores[name] = s
	}
	for _, name := range opts.PrimaryBackings {
		b.primBack[name] = true
	}
	if b.log == nil {
		b.log = slog.New(slog.DiscardHandler)
	}
	return b
}

// Primary returns the primary backend.
func (b *Backend) Primary() storage.Backend { return b.primary }

// configured reports whether backing can be used.
func (b *Backend) configured(backing string) bool {
	if backing == "" || b.primBack[backing] {
		return true
	}
	_, ok := b.stores[backing]
	return ok
}

// UnavailableBuckets returns the buckets whose backing is not configured
// (each mapped to its backing), for a startup warning.
func (b *Backend) UnavailableBuckets(ctx context.Context) (map[string]string, error) {
	infos, err := b.primary.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, info := range infos {
		if !b.configured(info.Backing) {
			out[info.Name] = info.Backing
		}
	}
	return out, nil
}

// backingOf returns the backing of bucket (cached; a bucket's backing never
// changes while it exists).
func (b *Backend) backingOf(ctx context.Context, bucket string) (string, error) {
	b.mu.RLock()
	backing, ok := b.cache[bucket]
	epoch := b.epoch
	b.mu.RUnlock()
	if ok {
		return backing, nil
	}
	info, err := b.primary.GetBucket(ctx, bucket)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	if b.epoch == epoch {
		b.cache[bucket] = info.Backing
	}
	b.mu.Unlock()
	return info.Backing, nil
}

func (b *Backend) forget(bucket string) {
	b.mu.Lock()
	delete(b.cache, bucket)
	b.epoch++
	b.mu.Unlock()
}

// store returns the DataStore holding bucket's objects, or nil when the
// primary holds them.
func (b *Backend) store(ctx context.Context, bucket string) (DataStore, error) {
	backing, err := b.backingOf(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if backing == "" || b.primBack[backing] {
		return nil, nil
	}
	if s, ok := b.stores[backing]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("%w: bucket %q is on backing %q, which is not configured", storage.ErrBackingUnavailable, bucket, backing)
}

// --- buckets ---

func (b *Backend) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	return b.primary.ListBuckets(ctx)
}

// CreateBucket creates a bucket on opts.Backing ("" = the default backing,
// "local" = the server's own storage). Unknown backings yield
// storage.ErrInvalidBacking; object lock on a DataStore backing yields
// storage.ErrNotSupported. A DataStore is prepared (AttachBucket) before
// the primary records the bucket.
func (b *Backend) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	backing := opts.Backing
	switch backing {
	case "":
		backing = b.def
	case BackingLocal:
		backing = ""
	}
	if !b.configured(backing) {
		return fmt.Errorf("%w: %q", storage.ErrInvalidBacking, backing)
	}
	opts.Backing = backing
	s := b.stores[backing]
	if s != nil {
		// An existing bucket is reported as such whatever the options (as
		// the primary does), before options the backing cannot honour.
		if _, err := b.primary.GetBucket(ctx, name); err == nil {
			return fmt.Errorf("%w: %q", storage.ErrBucketExists, name)
		} else if !errors.Is(err, storage.ErrBucketNotFound) {
			return err
		}
		if opts.ObjectLockEnabled {
			return fmt.Errorf("%w: object lock on backing %q", storage.ErrNotSupported, backing)
		}
		info := storage.BucketInfo{Name: name, Region: opts.Region, Owner: opts.Owner, Backing: backing}
		if err := s.AttachBucket(ctx, info); err != nil {
			return err
		}
	}
	b.forget(name)
	if err := b.primary.CreateBucket(ctx, name, opts); err != nil {
		return err
	}
	b.forget(name)
	return nil
}

func (b *Backend) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	return b.primary.GetBucket(ctx, name)
}

// DeleteBucket deletes an empty bucket. For a DataStore bucket the store
// must hold no objects; its uploads are aborted after the primary dropped
// the bucket.
func (b *Backend) DeleteBucket(ctx context.Context, name string) error {
	info, err := b.primary.GetBucket(ctx, name)
	if err != nil {
		return err
	}
	s := b.stores[info.Backing]
	if s != nil {
		if err := s.CheckEmpty(ctx, name); err != nil {
			return err
		}
	}
	err = b.primary.DeleteBucket(ctx, name)
	b.forget(name)
	if err != nil {
		return err
	}
	if s != nil {
		if err := s.DetachBucket(context.WithoutCancel(ctx), name); err != nil {
			b.log.Warn("routed: detach deleted bucket from its backing", "bucket", name, "backing", info.Backing, "err", err)
		}
	}
	return nil
}

func (b *Backend) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	s, err := b.store(ctx, name)
	if err != nil {
		return err
	}
	if s != nil {
		return fmt.Errorf("%w: versioning on a gateway backing", storage.ErrNotSupported)
	}
	return b.primary.SetBucketVersioning(ctx, name, status)
}

func (b *Backend) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	s, err := b.store(ctx, name)
	if err != nil {
		return err
	}
	if s != nil && enabled {
		return fmt.Errorf("%w: object lock on a gateway backing", storage.ErrNotSupported)
	}
	return b.primary.SetBucketObjectLock(ctx, name, enabled)
}

func (b *Backend) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	return b.primary.GetBucketConfig(ctx, bucket, key)
}

func (b *Backend) PutBucketConfig(ctx context.Context, bucket, key string, data []byte) error {
	return b.primary.PutBucketConfig(ctx, bucket, key, data)
}

func (b *Backend) DeleteBucketConfig(ctx context.Context, bucket, key string) error {
	return b.primary.DeleteBucketConfig(ctx, bucket, key)
}

// --- objects ---

func (b *Backend) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if s != nil {
		w, err := s.NewObjectWriter(ctx, bucket, key)
		if err != nil {
			return nil, err
		}
		return &storeWriter{ObjectWriter: w, b: b, bucket: bucket}, nil
	}
	return b.primary.NewObjectWriter(ctx, bucket, key)
}

func (b *Backend) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if s != nil {
		return s.GetObjectInfo(ctx, bucket, key, versionID)
	}
	return b.primary.GetObjectInfo(ctx, bucket, key, versionID)
}

func (b *Backend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	if s != nil {
		return s.OpenObject(ctx, bucket, key, versionID)
	}
	return b.primary.OpenObject(ctx, bucket, key, versionID)
}

func (b *Backend) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if s != nil {
		return s.UpdateObjectMeta(ctx, bucket, key, versionID, fn)
	}
	return b.primary.UpdateObjectMeta(ctx, bucket, key, versionID, fn)
}

func (b *Backend) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if s != nil {
		return s.DeleteObject(ctx, bucket, key, versionID)
	}
	return b.primary.DeleteObject(ctx, bucket, key, versionID)
}

// DeleteObjectIf implements storage.ConditionalDeleter. With a primary
// that does not implement it, primary buckets yield storage.ErrNotSupported
// (callers then fall back as for a backend without the extension).
func (b *Backend) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	if s != nil {
		return s.DeleteObjectIf(ctx, bucket, key, versionID, check)
	}
	cd, ok := b.primary.(storage.ConditionalDeleter)
	if !ok {
		return storage.DeleteResult{}, fmt.Errorf("%w: conditional delete", storage.ErrNotSupported)
	}
	return cd.DeleteObjectIf(ctx, bucket, key, versionID, check)
}

func (b *Backend) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return err
	}
	if s != nil {
		return s.WalkObjects(ctx, bucket, prefix, after, fn)
	}
	return b.primary.WalkObjects(ctx, bucket, prefix, after, fn)
}

func (b *Backend) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return err
	}
	if s != nil {
		return s.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn)
	}
	return b.primary.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn)
}

// --- multipart ---

func (b *Backend) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	if s != nil {
		return s.CreateMultipartUpload(ctx, bucket, key, meta, initiator)
	}
	return b.primary.CreateMultipartUpload(ctx, bucket, key, meta, initiator)
}

func (b *Backend) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	if s != nil {
		return s.GetMultipartUpload(ctx, bucket, key, uploadID)
	}
	return b.primary.GetMultipartUpload(ctx, bucket, key, uploadID)
}

func (b *Backend) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if s != nil {
		return s.NewPartWriter(ctx, bucket, key, uploadID, partNumber)
	}
	return b.primary.NewPartWriter(ctx, bucket, key, uploadID, partNumber)
}

func (b *Backend) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if s != nil {
		return s.ListParts(ctx, bucket, key, uploadID)
	}
	return b.primary.ListParts(ctx, bucket, key, uploadID)
}

func (b *Backend) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if s != nil {
		return s.ListMultipartUploads(ctx, bucket)
	}
	return b.primary.ListMultipartUploads(ctx, bucket)
}

// WalkMultipartUploads implements storage.UploadWalker.
func (b *Backend) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return err
	}
	if s != nil {
		return s.WalkMultipartUploads(ctx, bucket, prefix, after, fn)
	}
	return storage.WalkMultipartUploads(ctx, b.primary, bucket, prefix, after, fn)
}

func (b *Backend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if s != nil {
		return s.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, b.withConfigs(ctx, bucket, opts))
	}
	return b.primary.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, opts)
}

func (b *Backend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	s, err := b.store(ctx, bucket)
	if err != nil {
		return err
	}
	if s != nil {
		return s.AbortMultipartUpload(ctx, bucket, key, uploadID)
	}
	return b.primary.AbortMultipartUpload(ctx, bucket, key, uploadID)
}

// withConfigs makes opts.BeforePublish see the bucket's configuration
// documents, which the primary keeps (a DataStore passes none).
func (b *Backend) withConfigs(ctx context.Context, bucket string, opts storage.CommitOptions) storage.CommitOptions {
	hook := opts.BeforePublish
	if hook == nil {
		return opts
	}
	opts.BeforePublish = func(meta, current *storage.ObjectMeta, created time.Time, _ func(string) []byte) error {
		return hook(meta, current, created, func(key string) []byte {
			data, err := b.primary.GetBucketConfig(ctx, bucket, key)
			if err != nil {
				return nil
			}
			return data
		})
	}
	return opts
}

// storeWriter is a DataStore's object writer whose commits see the
// bucket's configuration documents (withConfigs).
type storeWriter struct {
	storage.ObjectWriter
	b      *Backend
	bucket string
}

func (w *storeWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	return w.ObjectWriter.Commit(ctx, meta, w.b.withConfigs(ctx, w.bucket, opts))
}

// Close closes the stores and then the primary, returning the first error.
// It is idempotent.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		var errs []error
		for name, s := range b.stores {
			if err := s.Close(); err != nil {
				errs = append(errs, fmt.Errorf("routed: close backing %q: %w", name, err))
			}
		}
		if err := b.primary.Close(); err != nil {
			errs = append(errs, err)
		}
		b.closeErr = errors.Join(errs...)
	})
	return b.closeErr
}
