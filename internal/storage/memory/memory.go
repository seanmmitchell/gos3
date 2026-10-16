// Package memory implements an ephemeral, in-memory storage.Backend.
//
// Everything lives in Go maps and per-bucket ordered indexes guarded by
// read/write mutexes, so the backend is safe for concurrent use. It is meant
// for tests, development and ephemeral deployments: all data is lost when
// the process exits.
//
// Design notes:
//   - Each bucket is an index.Bucket whose payload is the stored bytes: the
//     index package implements the versioning semantics (version IDs and
//     stamps, null versions, delete markers, conditional commits, imports,
//     walks and the multipart upload order), shared with the s3store
//     backend. This package adds locking, data handling and the
//     storage.Backend surface.
//   - Object data is stored as immutable byte slices; readers returned by
//     OpenObject are snapshots over the slice of the exact version opened and
//     are never affected by later writes or deletes.
//   - All metadata maps and slices are deep-copied on the way in and out, so
//     callers can never alias internal state.
//   - Walk callbacks run without any lock held (the walk collects small
//     batches under a read lock), so callbacks may call back into the backend,
//     including mutating it.
//   - Every version is stamped by the backend's index.Clock, so stamps (and
//     the version IDs generated from them) are unique and time-ordered
//     across the backend's buckets.
package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/index"
)

// ErrClosed is returned by every operation after Close.
var ErrClosed = errors.New("memory: backend closed")

// Backend is an in-memory storage.Backend. The zero value is not usable; use
// New.
type Backend struct {
	mu      sync.RWMutex // guards buckets and closed
	buckets map[string]*bucket
	closed  bool

	clock index.Clock

	// now returns the current time; replaceable in tests.
	now func() time.Time
}

var (
	_ storage.Backend                = (*Backend)(nil)
	_ storage.ConditionalDeleter     = (*Backend)(nil)
	_ storage.NewerVersionsDeleter   = (*Backend)(nil)
	_ storage.UploadWalker           = (*Backend)(nil)
	_ storage.ConditionalMetaUpdater = (*Backend)(nil)
	_ storage.ConfigViewer           = (*Backend)(nil)
	_ storage.CheckedDeleter         = (*Backend)(nil)
)

// New returns an empty in-memory backend.
func New() *Backend {
	return &Backend{
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
}

// bucket holds the state of one bucket: its index (info, configuration
// documents, versions with their data, uploads with their parts) guarded
// by mu.
type bucket struct {
	mu sync.RWMutex
	ix *index.Bucket[[]byte]
	// deleted is set when the bucket is removed (or the backend closed) so
	// that operations holding a stale pointer fail with ErrBucketNotFound.
	deleted bool
}

// nextStamp returns a strictly increasing stamp based on the wall clock.
func (b *Backend) nextStamp() (uint64, time.Time) {
	return b.clock.Next(b.now())
}

// --- lookup helpers ---

func checkCtx(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// lookup returns the bucket named name without locking it.
func (b *Backend) lookup(name string) (*bucket, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return nil, ErrClosed
	}
	bk, ok := b.buckets[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	}
	return bk, nil
}

// rlockBucket returns the bucket read-locked. The caller must RUnlock it.
func (b *Backend) rlockBucket(ctx context.Context, name string) (*bucket, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	bk, err := b.lookup(name)
	if err != nil {
		return nil, err
	}
	bk.mu.RLock()
	if bk.deleted {
		bk.mu.RUnlock()
		return nil, b.goneErr(name)
	}
	return bk, nil
}

// lockBucket returns the bucket write-locked. The caller must Unlock it.
func (b *Backend) lockBucket(ctx context.Context, name string) (*bucket, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	bk, err := b.lookup(name)
	if err != nil {
		return nil, err
	}
	bk.mu.Lock()
	if bk.deleted {
		bk.mu.Unlock()
		return nil, b.goneErr(name)
	}
	return bk, nil
}

// goneErr is the error for a bucket pointer found to be deleted.
func (b *Backend) goneErr(name string) error {
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
}

// relock re-acquires the write lock of a bucket pointer obtained earlier
// (by a writer) and verifies it still exists.
func (b *Backend) relock(ctx context.Context, bk *bucket) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	bk.mu.Lock()
	if bk.deleted {
		name := bk.ix.Info.Name
		bk.mu.Unlock()
		return b.goneErr(name)
	}
	return nil
}

// --- buckets ---

// ListBuckets returns all buckets sorted by name.
func (b *Backend) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return nil, ErrClosed
	}
	out := make([]storage.BucketInfo, 0, len(b.buckets))
	for _, bk := range b.buckets {
		bk.mu.RLock()
		out = append(out, bk.ix.Info)
		bk.mu.RUnlock()
	}
	slices.SortFunc(out, func(a, c storage.BucketInfo) int {
		switch {
		case a.Name < c.Name:
			return -1
		case a.Name > c.Name:
			return 1
		}
		return 0
	})
	return out, nil
}

// CreateBucket creates a bucket. Any non-empty name is accepted (validation
// is the S3 layer's job); "" yields ErrInvalidName.
func (b *Backend) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	if name == "" || !utf8.ValidString(name) {
		return fmt.Errorf("%w: bucket %q", storage.ErrInvalidName, name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if _, ok := b.buckets[name]; ok {
		return fmt.Errorf("%w: %q", storage.ErrBucketExists, name)
	}
	info := storage.BucketInfo{
		Name:              name,
		CreatedAt:         b.now().UTC(),
		Region:            opts.Region,
		ObjectLockEnabled: opts.ObjectLockEnabled,
		Owner:             opts.Owner,
	}
	if opts.ObjectLockEnabled {
		info.Versioning = storage.VersioningEnabled
	}
	ix := index.NewBucket[[]byte](info, b.nextStamp)
	b.buckets[name] = &bucket{ix: ix}
	return nil
}

// GetBucket returns the bucket's info.
func (b *Backend) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	bk, err := b.rlockBucket(ctx, name)
	if err != nil {
		return storage.BucketInfo{}, err
	}
	defer bk.mu.RUnlock()
	return bk.ix.Info, nil
}

// DeleteBucket removes an empty bucket and its configuration documents.
func (b *Backend) DeleteBucket(ctx context.Context, name string) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	bk, ok := b.buckets[name]
	if !ok {
		return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	}
	bk.mu.Lock()
	defer bk.mu.Unlock()
	if bk.ix.Len() > 0 {
		return fmt.Errorf("%w: %q", storage.ErrBucketNotEmpty, name)
	}
	// In-progress multipart uploads do not keep a bucket alive (as on
	// AWS); they are aborted with it.
	bk.deleted = true
	bk.ix.Configs = nil
	bk.ix.ClearUploads()
	delete(b.buckets, name)
	return nil
}

// SetBucketVersioning changes the versioning state. Reverting a bucket that
// has been versioned to VersioningUnversioned is rejected.
func (b *Backend) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	switch status {
	case storage.VersioningUnversioned, storage.VersioningEnabled, storage.VersioningSuspended:
	default:
		return fmt.Errorf("memory: invalid versioning status %q", status)
	}
	bk, err := b.lockBucket(ctx, name)
	if err != nil {
		return err
	}
	defer bk.mu.Unlock()
	if status == storage.VersioningUnversioned && bk.ix.Info.Versioning != storage.VersioningUnversioned {
		return errors.New("memory: a versioned bucket cannot become unversioned")
	}
	bk.ix.Info.Versioning = status
	return nil
}

// SetBucketObjectLock sets the bucket's object-lock flag.
func (b *Backend) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	bk, err := b.lockBucket(ctx, name)
	if err != nil {
		return err
	}
	defer bk.mu.Unlock()
	bk.ix.Info.ObjectLockEnabled = enabled
	return nil
}

// GetBucketConfig returns a copy of a configuration document.
func (b *Backend) GetBucketConfig(ctx context.Context, bucketName, key string) ([]byte, error) {
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return nil, err
	}
	defer bk.mu.RUnlock()
	data, ok := bk.ix.Configs[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", storage.ErrConfigNotFound, bucketName, key)
	}
	return bytes.Clone(data), nil
}

// ViewBucketConfig calls fn with a stored configuration document, without
// copying it, under the bucket's read lock (see storage.ConfigViewer).
func (b *Backend) ViewBucketConfig(ctx context.Context, bucketName, key string, fn func([]byte) error) error {
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return err
	}
	defer bk.mu.RUnlock()
	data, ok := bk.ix.Configs[key]
	if !ok {
		return fmt.Errorf("%w: %s/%s", storage.ErrConfigNotFound, bucketName, key)
	}
	return fn(data)
}

// PutBucketConfig stores a copy of a configuration document.
func (b *Backend) PutBucketConfig(ctx context.Context, bucketName, key string, data []byte) error {
	if key == "" {
		return fmt.Errorf("%w: empty config key", storage.ErrInvalidName)
	}
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return err
	}
	defer bk.mu.Unlock()
	bk.ix.Configs[key] = append([]byte{}, data...)
	return nil
}

// DeleteBucketConfig removes a configuration document; it is idempotent.
func (b *Backend) DeleteBucketConfig(ctx context.Context, bucketName, key string) error {
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return err
	}
	defer bk.mu.Unlock()
	delete(bk.ix.Configs, key)
	return nil
}

// Close discards all data. Every subsequent operation returns ErrClosed;
// open readers keep working. Close is idempotent.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for _, bk := range b.buckets {
		bk.mu.Lock()
		bk.deleted = true
		bk.mu.Unlock()
	}
	b.buckets = nil
	return nil
}
