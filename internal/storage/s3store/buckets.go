package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// ListBuckets returns all buckets sorted by name.
func (b *Backend) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	st, err := b.current(ctx)
	if err != nil {
		return nil, err
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]storage.BucketInfo, 0, len(st.buckets))
	for _, bk := range st.buckets {
		bk.mu.RLock()
		out = append(out, bk.ix.Info)
		bk.mu.RUnlock()
	}
	slices.SortFunc(out, func(a, c storage.BucketInfo) int { return strings.Compare(a.Name, c.Name) })
	return out, nil
}

// CreateBucket creates a bucket with a new random bucket ID.
func (b *Backend) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	if err := validBucketName(name); err != nil {
		return err
	}
	st, err := b.current(ctx)
	if err != nil {
		return err
	}
	st.mu.Lock()
	if st.dropped {
		st.mu.Unlock()
		return b.unavailableErr()
	}
	if _, ok := st.buckets[name]; ok {
		st.mu.Unlock()
		return fmt.Errorf("%w: %q", storage.ErrBucketExists, name)
	}
	info := storage.BucketInfo{
		Name:              name,
		CreatedAt:         b.now().UTC(),
		Region:            opts.Region,
		ObjectLockEnabled: opts.ObjectLockEnabled,
		Owner:             opts.Owner,
		Backing:           opts.Backing,
	}
	if opts.ObjectLockEnabled {
		info.Versioning = storage.VersioningEnabled
	}
	bk := b.newBucket(info, randomID(16))
	st.buckets[name] = bk
	done := b.appendLocked(bk, &record{Op: opMkBucket, B: name, ID: bk.id, Info: &info}, nil)
	st.mu.Unlock()
	return await(ctx, done)
}

// GetBucket returns the bucket's info.
func (b *Backend) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	bk, err := b.rlockBucket(ctx, name)
	if err != nil {
		return storage.BucketInfo{}, err
	}
	info, lsn := bk.ix.Info, bk.lsn
	bk.mu.RUnlock()
	return info, b.waitRead(ctx, lsn)
}

// DeleteBucket removes an empty bucket with its configuration documents
// and in-progress uploads.
func (b *Backend) DeleteBucket(ctx context.Context, name string) error {
	st, err := b.current(ctx)
	if err != nil {
		return err
	}
	st.mu.Lock()
	if st.dropped {
		st.mu.Unlock()
		return b.unavailableErr()
	}
	bk, ok := st.buckets[name]
	if !ok {
		st.mu.Unlock()
		return fmt.Errorf("%w: %q", storage.ErrBucketNotFound, name)
	}
	bk.mu.Lock()
	if bk.gone() {
		err := b.goneErr(bk)
		bk.mu.Unlock()
		st.mu.Unlock()
		return err
	}
	if bk.ix.Len() > 0 {
		bk.mu.Unlock()
		st.mu.Unlock()
		return fmt.Errorf("%w: %q", storage.ErrBucketNotEmpty, name)
	}
	var parts []string
	for _, u := range bk.ix.ClearUploads() {
		parts = append(parts, b.partBlobs(u)...)
	}
	bk.deleted = true
	delete(st.buckets, name)
	done := b.appendLocked(bk, &record{Op: opRmBucket, B: name}, b.dropOnDurable(parts))
	bk.mu.Unlock()
	st.mu.Unlock()
	return await(ctx, done)
}

// SetBucketVersioning changes the versioning state.
func (b *Backend) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	switch status {
	case storage.VersioningUnversioned, storage.VersioningEnabled, storage.VersioningSuspended:
	default:
		return fmt.Errorf("s3store: invalid versioning status %q", status)
	}
	bk, err := b.lockBucket(ctx, name)
	if err != nil {
		return err
	}
	if status == storage.VersioningUnversioned && bk.ix.Info.Versioning != storage.VersioningUnversioned {
		bk.mu.Unlock()
		return errors.New("s3store: a versioned bucket cannot become unversioned")
	}
	bk.ix.Info.Versioning = status
	ver := string(status)
	done := b.appendLocked(bk, &record{Op: opVersioning, B: name, Ver: &ver}, nil)
	bk.mu.Unlock()
	return await(ctx, done)
}

// SetBucketObjectLock sets the bucket's object-lock flag.
func (b *Backend) SetBucketObjectLock(ctx context.Context, name string, enabled bool) error {
	bk, err := b.lockBucket(ctx, name)
	if err != nil {
		return err
	}
	bk.ix.Info.ObjectLockEnabled = enabled
	done := b.appendLocked(bk, &record{Op: opObjectLock, B: name, Lock: &enabled}, nil)
	bk.mu.Unlock()
	return await(ctx, done)
}

// GetBucketConfig returns a copy of a configuration document.
func (b *Backend) GetBucketConfig(ctx context.Context, name, key string) ([]byte, error) {
	var out []byte
	err := b.ViewBucketConfig(ctx, name, key, func(data []byte) error {
		out = bytes.Clone(data)
		if out == nil {
			out = []byte{}
		}
		return nil
	})
	return out, err
}

// ViewBucketConfig calls fn with a stored configuration document without
// copying it (see storage.ConfigViewer). Documents are never modified in
// place, so fn runs without the bucket's lock, after the document is
// durable.
func (b *Backend) ViewBucketConfig(ctx context.Context, name, key string, fn func([]byte) error) error {
	bk, err := b.rlockBucket(ctx, name)
	if err != nil {
		return err
	}
	data, ok := bk.ix.Configs[key]
	lsn := bk.lsn
	bk.mu.RUnlock()
	if err := b.waitRead(ctx, lsn); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s/%s", storage.ErrConfigNotFound, name, key)
	}
	return fn(data)
}

// PutBucketConfig stores a copy of a configuration document.
func (b *Backend) PutBucketConfig(ctx context.Context, name, key string, data []byte) error {
	if key == "" {
		return fmt.Errorf("%w: empty config key", storage.ErrInvalidName)
	}
	bk, err := b.lockBucket(ctx, name)
	if err != nil {
		return err
	}
	doc := append([]byte{}, data...)
	bk.ix.Configs[key] = doc
	done := b.appendLocked(bk, &record{Op: opPutConfig, B: name, K: key, D: doc}, nil)
	bk.mu.Unlock()
	return await(ctx, done)
}

// DeleteBucketConfig removes a configuration document; it is idempotent.
func (b *Backend) DeleteBucketConfig(ctx context.Context, name, key string) error {
	bk, err := b.lockBucket(ctx, name)
	if err != nil {
		return err
	}
	if _, ok := bk.ix.Configs[key]; !ok {
		lsn := bk.lsn
		bk.mu.Unlock()
		return b.waitRead(ctx, lsn)
	}
	delete(bk.ix.Configs, key)
	done := b.appendLocked(bk, &record{Op: opDelConfig, B: name, K: key}, nil)
	bk.mu.Unlock()
	return await(ctx, done)
}
