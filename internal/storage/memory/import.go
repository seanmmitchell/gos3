package memory

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
)

var _ storage.VersionImporter = (*Backend)(nil)

// versionWriter is the storage.VersionWriter of the memory backend.
type versionWriter struct {
	b    *Backend
	bk   *bucket
	name string // bucket name
	key  string
	mu   sync.Mutex
	buf  []byte
	done bool
}

// NewVersionWriter starts writing an imported version of bucket/key (see
// storage.VersionImporter).
func (b *Backend) NewVersionWriter(ctx context.Context, bucketName, key string) (storage.VersionWriter, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return nil, err
	}
	bk.mu.RUnlock()
	return &versionWriter{b: b, bk: bk, name: bucketName, key: key}, nil
}

func (w *versionWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// Commit stores the written data as the newest version of the key with the
// identity given by info (see storage.VersionWriter): the version gets a
// fresh stamp, so it sorts newest, but keeps info's version ID and
// LastModified.
func (w *versionWriter) Commit(ctx context.Context, info storage.ObjectInfo) (storage.ObjectInfo, error) {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return storage.ObjectInfo{}, errWriterDone
	}
	w.done = true
	data := slices.Clip(w.buf)
	w.buf = nil
	w.mu.Unlock()

	if !storage.ValidImportedVersionID(info.VersionID) {
		return storage.ObjectInfo{}, fmt.Errorf("%w: version ID %.80q", storage.ErrInvalidName, info.VersionID)
	}
	if info.DeleteMarker && len(data) > 0 {
		return storage.ObjectInfo{}, fmt.Errorf("%w: delete marker %s/%q with data", storage.ErrInvalidName, w.name, w.key)
	}
	if err := w.b.relock(ctx, w.bk); err != nil {
		return storage.ObjectInfo{}, err
	}
	defer w.bk.mu.Unlock()

	if data == nil {
		data = []byte{}
	}
	v := &version{
		id:           info.VersionID,
		deleteMarker: info.DeleteMarker,
		imported:     info.VersionID != storage.NullVersionID,
		data:         data,
		lastModified: info.LastModified.UTC(),
	}
	if !info.DeleteMarker {
		v.meta = cloneMeta(info.ObjectMeta)
	}
	v.stamp, _ = w.b.nextStamp()

	obj, ok := w.bk.objects.Get(w.key)
	if !ok {
		obj = &object{}
		w.bk.objects.Set(w.key, obj)
	} else if i := obj.indexByID(v.id); i >= 0 {
		// Replaced wherever it is in the history; obj keeps at least the
		// new version, so it stays in the bucket.
		obj.removeAt(i)
	}
	obj.versions = append(obj.versions, v)
	if v.imported {
		obj.imported++
	}
	return v.info(w.bk.info.Name, w.key, true), nil
}

func (w *versionWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	w.buf = nil
	return nil
}
