package s3account

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// Native buckets
//
// The native store (internal/storage/native) serves the objects of every
// native bucket; the registry keeps their records: the binding (remote
// bucket and region, whether gos3 created it), the bucket info with the
// remote's versioning and object lock state as last recorded, the
// capabilities the remote was probed with, and the configuration
// documents. The store resolves buckets through resolveNative on every
// operation, so a recorded change applies at once. When an answer of the
// remote contradicts the record (OnDrift), the bucket is refreshed in the
// background (rate-limited) and its record updated.

// NativeClientSetter is implemented by native stores that can switch to a
// new client while open. PrepareConnection's commit uses it; a store
// without it is reopened with the new connection instead (operations
// running on the old store finish on it).
type NativeClientSetter interface {
	SetClient(c *s3client.Client, partSize int64)
}

// openNative opens the native store with conn.
func (b *Backend) openNative(conn *s3store.Connection) (NativeStore, error) {
	o := native.Options{
		Name:     b.name,
		Client:   conn.Client,
		State:    b.natState,
		Resolve:  b.resolveNative,
		OnDrift:  b.onDrift,
		PartSize: conn.PartSize,
		Logger:   b.log,
	}
	if b.opts.TempDir != "" {
		o.SpoolDir = filepath.Join(b.opts.TempDir, "spool")
	}
	if b.opts.TuneNative != nil {
		b.opts.TuneNative(&o)
	}
	if b.hooks.openNative != nil {
		return b.hooks.openNative(o)
	}
	return native.Open(o)
}

// resolveNative is the native store's Options.Resolve.
func (b *Backend) resolveNative(_ context.Context, bucket string) (native.Bucket, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.reg != nil {
		if e := b.reg.buckets[bucket]; e != nil && e.Kind == storage.BucketKindNative {
			if e.claimErr != nil {
				return native.Bucket{}, fmt.Errorf("s3account %q: bucket %q: %w: %w", b.name, bucket, storage.ErrBucketUnavailable, e.claimErr)
			}
			return native.Bucket{Binding: e.binding(), Versioning: e.Info.Versioning, ObjectLock: e.Info.ObjectLockEnabled}, nil
		}
	}
	if p, ok := b.pending[bucket]; ok {
		return p, nil
	}
	return native.Bucket{}, fmt.Errorf("%w: %q", storage.ErrBucketNotFound, bucket)
}

// nativeConfig returns a native bucket's configuration document (nil when
// unset). Documents are never modified in place, so it is not copied.
func (b *Backend) nativeConfig(bucket, key string) []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.reg == nil {
		return nil
	}
	return b.reg.configs[bucket][key]
}

// withConfigs makes opts.BeforePublish see a native bucket's
// configuration documents, which the registry keeps (the native store
// passes none).
func (b *Backend) withConfigs(bucket string, opts storage.CommitOptions) storage.CommitOptions {
	hook := opts.BeforePublish
	if hook == nil {
		return opts
	}
	opts.BeforePublish = func(meta, current *storage.ObjectMeta, created time.Time, _ func(string) []byte) error {
		return hook(meta, current, created, func(key string) []byte { return b.nativeConfig(bucket, key) })
	}
	return opts
}

// nativeWriter is a native store's object writer whose commits see the
// bucket's configuration documents (withConfigs).
type nativeWriter struct {
	storage.ObjectWriter
	b      *Backend
	bucket string
}

func (w *nativeWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	return w.ObjectWriter.Commit(ctx, meta, w.b.withConfigs(w.bucket, opts))
}

// --- drift ---

// onDrift is the native store's Options.OnDrift: it schedules a refresh of
// the bucket, at most one per driftInterval.
func (b *Backend) onDrift(bucket, reason string) {
	b.mu.Lock()
	last, scheduled := b.refreshing[bucket]
	if scheduled && (last.IsZero() || time.Since(last) < b.hooks.driftInterval) {
		b.mu.Unlock()
		return
	}
	b.refreshing[bucket] = time.Time{} // running
	b.mu.Unlock()
	if !b.spawn(func() { b.refreshDrifted(bucket, reason) }) {
		b.mu.Lock()
		delete(b.refreshing, bucket)
		b.mu.Unlock()
	}
}

// refreshDrifted refreshes bucket after a drift report.
func (b *Backend) refreshDrifted(bucket, reason string) {
	ctx, cancel := context.WithTimeout(b.ctx, time.Minute)
	defer cancel()
	err := b.refreshNative(ctx, bucket)
	b.mu.Lock()
	b.refreshing[bucket] = time.Now()
	b.mu.Unlock()
	if err != nil && b.ctx.Err() == nil {
		b.log.Warn("s3account: refreshing a native bucket after a drift report failed", "backing", b.name,
			"bucket", bucket, "reason", reason, "err", err)
	}
}

// refreshNative re-reads a native bucket's remote state and records it.
func (b *Backend) refreshNative(ctx context.Context, bucket string) error {
	b.mu.RLock()
	nat := b.nat
	b.mu.RUnlock()
	if nat == nil {
		return b.unavailable()
	}
	rs, err := nat.Refresh(ctx, bucket)
	if err != nil {
		return err
	}
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	b.mu.RLock()
	var rec bucketRecord
	e := b.reg.buckets[bucket]
	if e != nil {
		rec = e.bucketRecord
	}
	lock := b.reg.configs[bucket][storage.ConfigObjectLock]
	b.mu.RUnlock()
	if e == nil || rec.Kind != storage.BucketKindNative || !equalFoldRemote(rec.Remote, rs.Remote) {
		return nil // deleted or rebound meanwhile
	}
	next := applyRemoteState(rec, rs)
	if !sameRecord(rec, next) {
		b.log.Info("s3account: native bucket updated from its remote bucket", "backing", b.name, "bucket", bucket,
			"versioning", next.Info.Versioning, "objectLock", next.Info.ObjectLockEnabled, "region", next.RemoteRegion)
		if err := b.putBucket(ctx, bucket, next, nil); err != nil {
			return err
		}
	}
	if rs.LockConfig != nil && lock == nil {
		return b.putConfig(ctx, bucket, storage.ConfigObjectLock, rs.LockConfig)
	}
	return nil
}

// applyRemoteState returns rec updated with a remote's state.
func applyRemoteState(rec bucketRecord, rs native.RemoteState) bucketRecord {
	if rs.Region != "" {
		rec.RemoteRegion = rs.Region
	}
	if rs.Caps.Versioning {
		rec.Info.Versioning = rs.Versioning
	}
	if rs.Caps.ObjectLock {
		rec.Info.ObjectLockEnabled = rs.ObjectLock
		if rs.ObjectLock {
			rec.Info.Versioning = storage.VersioningEnabled
		}
	}
	caps := rs.Caps
	rec.Caps = &caps
	return rec
}

// sameRecord compares the parts of a native bucket record a refresh
// changes.
func sameRecord(a, b bucketRecord) bool {
	same := a.RemoteRegion == b.RemoteRegion && a.Info.Versioning == b.Info.Versioning &&
		a.Info.ObjectLockEnabled == b.Info.ObjectLockEnabled
	switch {
	case a.Caps == nil || b.Caps == nil:
		return same && a.Caps == b.Caps
	}
	return same && *a.Caps == *b.Caps
}

// equalFoldRemote compares remote bucket names (case-insensitively).
func equalFoldRemote(a, b string) bool { return strings.EqualFold(a, b) }

// --- connection ---

// switchNative makes the native store use conn (PrepareConnection's
// commit; adminMu held).
func (b *Backend) switchNative(conn *s3store.Connection) {
	b.mu.RLock()
	nat := b.nat
	b.mu.RUnlock()
	if nat == nil {
		return // opened with conn when the records load
	}
	if cs, ok := nat.(NativeClientSetter); ok {
		cs.SetClient(conn.Client, conn.PartSize)
		return
	}
	next, err := b.openNative(conn)
	if err != nil {
		b.log.Error("s3account: reopening the native store with the new connection failed; native buckets keep the old one until restart",
			"backing", b.name, "err", err)
		return
	}
	b.mu.Lock()
	b.nat = next
	b.mu.Unlock()
	if err := nat.Close(); err != nil {
		b.log.Warn("s3account: close the native store of the old connection", "backing", b.name, "err", err)
	}
}
