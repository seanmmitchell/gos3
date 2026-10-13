package memory

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
)

var (
	_ storage.VersionImporter     = (*Backend)(nil)
	_ storage.CompressingImporter = (*Backend)(nil)
)

// versionWriter is the storage.VersionWriter of the memory backend.
type versionWriter struct {
	b    *Backend
	bk   *bucket
	name string // bucket name
	key  string
	mu   sync.Mutex
	buf  []byte
	done bool
	// zw compresses into buf when the version is stored compressed.
	zw          *compress.Writer
	compression string
}

// NewVersionWriter starts writing an imported version of bucket/key (see
// storage.VersionImporter).
func (b *Backend) NewVersionWriter(ctx context.Context, bucketName, key string) (storage.VersionWriter, error) {
	return b.NewVersionWriterWith(ctx, bucketName, key, storage.WriteOptions{})
}

// NewVersionWriterWith is NewVersionWriter with write options (see
// storage.CompressingImporter).
func (b *Backend) NewVersionWriterWith(ctx context.Context, bucketName, key string, opts storage.WriteOptions) (storage.VersionWriter, error) {
	if err := checkCompression(opts.Compression); err != nil {
		return nil, err
	}
	if err := validKey(key); err != nil {
		return nil, err
	}
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return nil, err
	}
	bk.mu.RUnlock()
	w := &versionWriter{b: b, bk: bk, name: bucketName, key: key}
	if opts.Compression.Enabled() {
		w.compression = opts.Compression.Algorithm
		if w.zw, err = compress.NewWriter((*appendWriter)(&w.buf), opts.Compression.Level); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (w *versionWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	if w.zw != nil {
		return w.zw.Write(p)
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
	var enc stored
	if w.zw != nil {
		if err := w.zw.Close(); err != nil {
			w.buf = nil
			w.mu.Unlock()
			return storage.ObjectInfo{}, err
		}
		if w.zw.Size() > 0 {
			enc = stored{compression: w.compression, size: w.zw.Size()}
		}
	}
	data := slices.Clip(w.buf)
	w.buf = nil
	w.mu.Unlock()
	contentSize := int64(len(data))
	if enc.compression != "" {
		contentSize = enc.size
	}

	if !storage.ValidImportedVersionID(info.VersionID) {
		return storage.ObjectInfo{}, fmt.Errorf("%w: version ID %.80q", storage.ErrInvalidName, info.VersionID)
	}
	if info.DeleteMarker && contentSize > 0 {
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
		compression:  enc.compression,
		size:         enc.size,
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
	if !w.done && w.zw != nil {
		w.zw.Abandon()
	}
	w.done = true
	w.buf = nil
	return nil
}
