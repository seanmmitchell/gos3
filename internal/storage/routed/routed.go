// Package routed implements the storage.Backend of a gateway backing (see
// docs/backings.md, "The gateway option"): bucket records and bucket
// configuration documents live in a primary backend, objects and
// multipart uploads in a DataStore.
//
// In the server the primary is an s3store opened in gateway mode on the
// remote's metadata bucket (it keeps buckets and configurations and
// refuses object operations) and the DataStore is a gateway store
// (internal/storage/gateway) mapping each bucket to a remote bucket of the
// same name.
//
// Every object and multipart operation goes to the DataStore once the
// primary confirmed that the bucket exists; bucket and configuration
// operations go to the primary. Gateway buckets cannot be versioned or use
// object lock (storage.ErrNotSupported), store no encryption provider, and
// the Backend does not implement storage.VersionImporter, so it can be
// neither a replica nor the target of a bucket move.
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

// Options configures New.
type Options struct {
	// Primary stores bucket records and configuration documents.
	// Required.
	Primary storage.Backend
	// Store holds the objects and multipart uploads. Required.
	Store DataStore
	// Logger receives non-fatal errors. nil discards logs.
	Logger *slog.Logger
}

// Backend routes bucket operations to the primary and object operations
// to the DataStore. Create it with New.
type Backend struct {
	primary storage.Backend
	store   DataStore
	log     *slog.Logger

	mu sync.RWMutex
	// known caches the buckets the primary reported as existing, so object
	// operations do not each ask it (it may be remote).
	known map[string]bool
	// epoch counts cache invalidations; a lookup only caches its result
	// when no invalidation happened since it started, so a lookup racing
	// a DeleteBucket cannot re-insert the deleted bucket.
	epoch uint64

	closeOnce sync.Once
	closeErr  error
}

var (
	_ storage.Backend                    = (*Backend)(nil)
	_ storage.ConditionalDeleter         = (*Backend)(nil)
	_ storage.UploadWalker               = (*Backend)(nil)
	_ storage.ConfigViewer               = (*Backend)(nil)
	_ storage.EncryptionBackend          = (*Backend)(nil)
	_ storage.EncryptionProvidersBackend = (*Backend)(nil)
)

// New returns a routing backend. It takes ownership of the primary and the
// store: Close closes them.
func New(opts Options) *Backend {
	if opts.Primary == nil {
		panic("routed: Options.Primary is required")
	}
	if opts.Store == nil {
		panic("routed: Options.Store is required")
	}
	b := &Backend{
		primary: opts.Primary,
		store:   opts.Store,
		log:     opts.Logger,
		known:   make(map[string]bool),
	}
	if b.log == nil {
		b.log = slog.New(slog.DiscardHandler)
	}
	return b
}

// Primary returns the primary backend.
func (b *Backend) Primary() storage.Backend { return b.primary }

// Store returns the data store.
func (b *Backend) Store() DataStore { return b.store }

// exists returns nil when bucket exists, else the primary's error
// (storage.ErrBucketNotFound). Positive answers are cached until the
// bucket is deleted through this Backend.
func (b *Backend) exists(ctx context.Context, bucket string) error {
	b.mu.RLock()
	ok := b.known[bucket]
	epoch := b.epoch
	b.mu.RUnlock()
	if ok {
		return nil
	}
	if _, err := b.primary.GetBucket(ctx, bucket); err != nil {
		return err
	}
	b.mu.Lock()
	if b.epoch == epoch {
		b.known[bucket] = true
	}
	b.mu.Unlock()
	return nil
}

func (b *Backend) forget(bucket string) {
	b.mu.Lock()
	delete(b.known, bucket)
	b.epoch++
	b.mu.Unlock()
}

// --- buckets ---

func (b *Backend) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	return b.primary.ListBuckets(ctx)
}

// CreateBucket prepares the store (AttachBucket) and then records the
// bucket in the primary. An existing bucket yields storage.ErrBucketExists
// whatever the options; object lock yields storage.ErrNotSupported.
// opts.Backing is recorded as given.
func (b *Backend) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	// An existing bucket is reported as such whatever the options (as the
	// primary does), before options the store cannot honour.
	if _, err := b.primary.GetBucket(ctx, name); err == nil {
		return fmt.Errorf("%w: %q", storage.ErrBucketExists, name)
	} else if !errors.Is(err, storage.ErrBucketNotFound) {
		return err
	}
	if opts.ObjectLockEnabled {
		return fmt.Errorf("%w: object lock on a gateway backing", storage.ErrNotSupported)
	}
	info := storage.BucketInfo{Name: name, Region: opts.Region, Owner: opts.Owner, Backing: opts.Backing}
	if err := b.store.AttachBucket(ctx, info); err != nil {
		return err
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

// DeleteBucket deletes a bucket whose store side holds no objects; its
// uploads are aborted (DetachBucket) after the primary dropped the bucket.
func (b *Backend) DeleteBucket(ctx context.Context, name string) error {
	if _, err := b.primary.GetBucket(ctx, name); err != nil {
		return err
	}
	if err := b.store.CheckEmpty(ctx, name); err != nil {
		return err
	}
	err := b.primary.DeleteBucket(ctx, name)
	b.forget(name)
	if err != nil {
		return err
	}
	if err := b.store.DetachBucket(context.WithoutCancel(ctx), name); err != nil {
		b.log.Warn("routed: detach deleted bucket from the data store", "bucket", name, "err", err)
	}
	return nil
}

// SetBucketVersioning yields storage.ErrNotSupported for an existing
// bucket: gateway buckets stay unversioned.
func (b *Backend) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	if err := b.exists(ctx, name); err != nil {
		return err
	}
	return fmt.Errorf("%w: versioning on a gateway backing", storage.ErrNotSupported)
}

// SetBucketObjectLock yields storage.ErrNotSupported when enabling object
// lock; disabling it is passed to the primary.
func (b *Backend) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	if err := b.exists(ctx, name); err != nil {
		return err
	}
	if enabled {
		return fmt.Errorf("%w: object lock on a gateway backing", storage.ErrNotSupported)
	}
	return b.primary.SetBucketObjectLock(ctx, name, enabled)
}

func (b *Backend) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	return b.primary.GetBucketConfig(ctx, bucket, key)
}

// ViewBucketConfig implements storage.ConfigViewer with the primary's
// implementation, or a copy from GetBucketConfig when it has none.
func (b *Backend) ViewBucketConfig(ctx context.Context, bucket, key string, fn func(data []byte) error) error {
	if cv, ok := b.primary.(storage.ConfigViewer); ok {
		return cv.ViewBucketConfig(ctx, bucket, key, fn)
	}
	data, err := b.primary.GetBucketConfig(ctx, bucket, key)
	if err != nil {
		return err
	}
	return fn(data)
}

func (b *Backend) PutBucketConfig(ctx context.Context, bucket, key string, data []byte) error {
	return b.primary.PutBucketConfig(ctx, bucket, key, data)
}

func (b *Backend) DeleteBucketConfig(ctx context.Context, bucket, key string) error {
	return b.primary.DeleteBucketConfig(ctx, bucket, key)
}

// --- encryption ---

// EncryptionProviders implements storage.EncryptionProvidersBackend: a
// gateway stores objects natively, so no SSE-S3 provider is usable (an
// empty list).
func (b *Backend) EncryptionProviders(ctx context.Context, bucket string) ([]string, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return nil, err
	}
	return []string{}, nil
}

// PrepareEncryption implements storage.EncryptionBackend: it fails with
// errors.ErrUnsupported for every mode.
func (b *Backend) PrepareEncryption(ctx context.Context, bucket, mode string) error {
	if err := b.exists(ctx, bucket); err != nil {
		return err
	}
	return fmt.Errorf("routed: a gateway backing cannot store %s-encrypted objects: %w", mode, errors.ErrUnsupported)
}

// ValidateEncryptionSettings implements storage.EncryptionBackend: it
// fails with errors.ErrUnsupported for every provider.
func (b *Backend) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	if err := b.exists(ctx, bucket); err != nil {
		return err
	}
	return fmt.Errorf("routed: a gateway backing cannot store %s-encrypted objects: %w", cfg.Provider, errors.ErrUnsupported)
}

// --- objects ---

func (b *Backend) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return nil, err
	}
	w, err := b.store.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	return &storeWriter{ObjectWriter: w, b: b, bucket: bucket}, nil
}

func (b *Backend) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return storage.ObjectInfo{}, err
	}
	return b.store.GetObjectInfo(ctx, bucket, key, versionID)
}

func (b *Backend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	return b.store.OpenObject(ctx, bucket, key, versionID)
}

func (b *Backend) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return storage.ObjectInfo{}, err
	}
	return b.store.UpdateObjectMeta(ctx, bucket, key, versionID, fn)
}

func (b *Backend) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return storage.DeleteResult{}, err
	}
	return b.store.DeleteObject(ctx, bucket, key, versionID)
}

// DeleteObjectIf implements storage.ConditionalDeleter.
func (b *Backend) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return storage.DeleteResult{}, err
	}
	return b.store.DeleteObjectIf(ctx, bucket, key, versionID, check)
}

func (b *Backend) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if err := b.exists(ctx, bucket); err != nil {
		return err
	}
	return b.store.WalkObjects(ctx, bucket, prefix, after, fn)
}

func (b *Backend) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	if err := b.exists(ctx, bucket); err != nil {
		return err
	}
	return b.store.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn)
}

// --- multipart ---

func (b *Backend) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return storage.UploadInfo{}, err
	}
	return b.store.CreateMultipartUpload(ctx, bucket, key, meta, initiator)
}

func (b *Backend) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return storage.UploadInfo{}, err
	}
	return b.store.GetMultipartUpload(ctx, bucket, key, uploadID)
}

func (b *Backend) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return nil, err
	}
	return b.store.NewPartWriter(ctx, bucket, key, uploadID, partNumber)
}

func (b *Backend) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return nil, err
	}
	return b.store.ListParts(ctx, bucket, key, uploadID)
}

func (b *Backend) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return nil, err
	}
	return b.store.ListMultipartUploads(ctx, bucket)
}

// WalkMultipartUploads implements storage.UploadWalker.
func (b *Backend) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	if err := b.exists(ctx, bucket); err != nil {
		return err
	}
	return b.store.WalkMultipartUploads(ctx, bucket, prefix, after, fn)
}

func (b *Backend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	if err := b.exists(ctx, bucket); err != nil {
		return storage.ObjectInfo{}, err
	}
	return b.store.CompleteMultipartUpload(ctx, bucket, key, uploadID, partNumbers, meta, b.withConfigs(ctx, bucket, opts))
}

func (b *Backend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	if err := b.exists(ctx, bucket); err != nil {
		return err
	}
	return b.store.AbortMultipartUpload(ctx, bucket, key, uploadID)
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

// Close closes the store and then the primary, returning their errors
// joined. It is idempotent.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		var errs []error
		if err := b.store.Close(); err != nil {
			errs = append(errs, fmt.Errorf("routed: close data store: %w", err))
		}
		if err := b.primary.Close(); err != nil {
			errs = append(errs, err)
		}
		b.closeErr = errors.Join(errs...)
	})
	return b.closeErr
}
