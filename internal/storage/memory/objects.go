package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
	"github.com/seanmmitchell/gos3/internal/storage/index"
)

// errWriterDone is returned when writing to or committing a writer that was
// already committed or aborted.
var errWriterDone = errors.New("memory: writer already committed or aborted")

// errReaderClosed is returned by reads on a closed object reader.
var errReaderClosed = errors.New("memory: read on closed object reader")

// validKey reports whether the backend can store key.
func validKey(key string) error { return index.ValidKey(key) }

// --- writer ---

type objectWriter struct {
	b    *Backend
	bk   *bucket
	key  string
	mu   sync.Mutex
	buf  []byte
	done bool
	// zw compresses into buf when the version is stored compressed.
	zw          *compress.Writer
	compression string
	encryption  string
}

// The optional storage interface implemented by NewObjectWriterWith and
// CompleteMultipartUploadWith.
var _ storage.CompressingBackend = (*Backend)(nil)

// NewObjectWriter starts writing a new version of bucket/key.
func (b *Backend) NewObjectWriter(ctx context.Context, bucketName, key string) (storage.ObjectWriter, error) {
	return b.NewObjectWriterWith(ctx, bucketName, key, storage.WriteOptions{})
}

// NewObjectWriterWith is NewObjectWriter with write options: with
// opts.Compression the data is kept in the compress package's format (see
// storage.CompressingBackend).
func (b *Backend) NewObjectWriterWith(ctx context.Context, bucketName, key string, opts storage.WriteOptions) (storage.ObjectWriter, error) {
	if opts.Encryption != "" && opts.Encryption != storage.EncryptionAES256 {
		return nil, errors.ErrUnsupported
	}
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
	w := &objectWriter{b: b, bk: bk, key: key, encryption: opts.Encryption}
	if opts.Compression.Enabled() {
		w.compression = opts.Compression.Algorithm
		if w.zw, err = compress.NewWriter((*appendWriter)(&w.buf), opts.Compression.Level); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (w *objectWriter) Write(p []byte) (int, error) {
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

// appendWriter is an io.Writer appending to a byte slice.
type appendWriter []byte

func (a *appendWriter) Write(p []byte) (int, error) {
	*a = append(*a, p...)
	return len(p), nil
}

// checkCompression validates c before any data is written.
func checkCompression(c storage.Compression) error {
	switch c.Algorithm {
	case "":
		return nil
	case storage.CompressionDeflate:
		if !compress.ValidLevel(c.Level) {
			return fmt.Errorf("memory: invalid deflate level %d", c.Level)
		}
		return nil
	default:
		return fmt.Errorf("memory: unsupported compression algorithm %q", c.Algorithm)
	}
}

// stored is how the data of a new version is kept: compressed (algorithm
// set, size = content size) or as is (zero value).
type stored struct {
	compression string
	size        int64
}

// content describes data, kept as enc says, for the index; nil data is
// stored as an empty slice.
func content(data []byte, enc stored) index.Content[[]byte] {
	if data == nil {
		data = []byte{}
	}
	c := index.Content[[]byte]{Data: data, Size: int64(len(data))}
	if enc.compression != "" {
		c.Compression, c.Size, c.StoredSize = enc.compression, enc.size, int64(len(data))
	}
	return c
}

func (w *objectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
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

	if err := w.b.relock(ctx, w.bk); err != nil {
		return storage.ObjectInfo{}, err
	}
	defer w.bk.mu.Unlock()
	meta.Encryption = w.encryption
	return w.b.commitLocked(w.bk, w.key, data, enc, meta, opts)
}

func (w *objectWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done && w.zw != nil {
		w.zw.Abandon()
	}
	w.done = true
	w.buf = nil
	return nil
}

// commitLocked publishes data (stored as enc says) as a new version of key
// in bk, applying conditional checks and versioning semantics (see
// index.Bucket.Commit). bk must be write-locked; data is taken over by the
// backend and must not be modified afterwards.
func (b *Backend) commitLocked(bk *bucket, key string, data []byte, enc stored, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	info, _, err := bk.ix.Commit(key, content(data, enc), meta, opts)
	return info, err
}

// GetObjectInfo returns metadata for a version ("" = latest, which may be a
// delete marker). A missing key yields ErrObjectNotFound when versionID is
// "" and ErrVersionNotFound otherwise.
func (b *Backend) GetObjectInfo(ctx context.Context, bucketName, key, versionID string) (storage.ObjectInfo, error) {
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	defer bk.mu.RUnlock()
	v, latest, err := bk.ix.Find(key, versionID)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return v.Info(bk.ix.Info.Name, key, latest), nil
}

// objectReader is an immutable snapshot reader over one version's data.
type objectReader struct {
	mu     sync.Mutex
	r      bytes.Reader
	closed bool
}

func newObjectReader(data []byte) *objectReader {
	o := &objectReader{}
	o.r.Reset(data)
	return o
}

func (o *objectReader) Read(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errReaderClosed
	}
	return o.r.Read(p)
}

func (o *objectReader) Seek(offset int64, whence int) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errReaderClosed
	}
	return o.r.Seek(offset, whence)
}

// ReadAt implements io.ReaderAt.
func (o *objectReader) ReadAt(p []byte, off int64) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errReaderClosed
	}
	return o.r.ReadAt(p, off)
}

// WriteTo implements io.WriterTo for efficient io.Copy.
func (o *objectReader) WriteTo(w io.Writer) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errReaderClosed
	}
	return o.r.WriteTo(w)
}

func (o *objectReader) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	return nil
}

// OpenObject returns a version's info and a reader over its content. The
// reader is nil for delete markers.
func (b *Backend) OpenObject(ctx context.Context, bucketName, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	defer bk.mu.RUnlock()
	v, latest, err := bk.ix.Find(key, versionID)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	info := v.Info(bk.ix.Info.Name, key, latest)
	if v.DeleteMarker {
		return info, nil, nil
	}
	if v.Compression != "" {
		z, err := compress.NewReader(bytes.NewReader(v.Data), int64(len(v.Data)))
		if err != nil {
			return storage.ObjectInfo{}, nil, fmt.Errorf("memory: open %s/%s: %w", bucketName, key, err)
		}
		return info, z, nil
	}
	return info, newObjectReader(v.Data), nil
}

// UpdateObjectMeta atomically mutates the metadata of an existing version.
// fn receives a private copy; if it returns an error nothing changes. fn is
// called with the bucket locked and must not call back into the backend.
// Addressing a delete marker yields ErrObjectNotFound (wrapped with a
// "delete marker" message).
func (b *Backend) UpdateObjectMeta(ctx context.Context, bucketName, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	return b.updateObjectMeta(ctx, bucketName, key, versionID, nil, fn)
}

// UpdateObjectMetaIf is UpdateObjectMeta made conditional on check, which is
// called with the addressed version under the bucket's write lock (see
// storage.ConditionalMetaUpdater).
func (b *Backend) UpdateObjectMetaIf(ctx context.Context, bucketName, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if check == nil {
		return storage.ObjectInfo{}, errors.New("memory: nil update check")
	}
	return b.updateObjectMeta(ctx, bucketName, key, versionID, check, fn)
}

// updateObjectMeta implements UpdateObjectMeta and, with a non-nil check,
// UpdateObjectMetaIf.
func (b *Backend) updateObjectMeta(ctx context.Context, bucketName, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if fn == nil {
		return storage.ObjectInfo{}, errors.New("memory: nil update function")
	}
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	defer bk.mu.Unlock()
	info, _, err := bk.ix.UpdateMeta(key, versionID, check, fn)
	return info, err
}

// DeleteObject deletes a version or creates a delete marker per S3 semantics
// (see storage.Backend). In an Unversioned bucket the result is always
// DeleteResult{} (S3 reports no version information there).
func (b *Backend) DeleteObject(ctx context.Context, bucketName, key, versionID string) (storage.DeleteResult, error) {
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	defer bk.mu.Unlock()
	res, _, err := bk.ix.Delete(key, versionID)
	return res, err
}

// DeleteObjectIf is DeleteObject made conditional on check, which is called
// with the addressed version under the bucket's write lock (see
// storage.ConditionalDeleter).
func (b *Backend) DeleteObjectIf(ctx context.Context, bucketName, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("memory: nil delete check")
	}
	return b.deleteChecked(ctx, bucketName, key, versionID, func(o storage.ObjectInfo, _ bool) error { return check(o) }, false)
}

// DeleteVersionIf permanently deletes a version when check accepts it and
// the versions newer than it, under the bucket's write lock (see
// storage.NewerVersionsDeleter).
func (b *Backend) DeleteVersionIf(ctx context.Context, bucketName, key, versionID string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("memory: nil delete check")
	}
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	defer bk.mu.Unlock()
	res, _, err := bk.ix.DeleteVersionIf(key, versionID, limit, check)
	return res, err
}

// DeleteObjectChecked is DeleteObjectIf whose check is also consulted
// when the key has no versions (see storage.CheckedDeleter).
func (b *Backend) DeleteObjectChecked(ctx context.Context, bucketName, key, versionID string, check func(storage.ObjectInfo, bool) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("memory: nil delete check")
	}
	return b.deleteChecked(ctx, bucketName, key, versionID, check, true)
}

// deleteChecked implements DeleteObjectIf and, with absentOK,
// DeleteObjectChecked.
func (b *Backend) deleteChecked(ctx context.Context, bucketName, key, versionID string, check func(storage.ObjectInfo, bool) error, absentOK bool) (storage.DeleteResult, error) {
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	defer bk.mu.Unlock()
	res, _, err := bk.ix.DeleteIf(key, versionID, check, absentOK)
	return res, err
}

// --- walks ---

// walkAcquire returns the acquire function of the index walk helpers for
// a bucket: each call looks the bucket up and read-locks it.
func (b *Backend) walkAcquire(ctx context.Context, bucketName string) func() (*index.Bucket[[]byte], func(), error) {
	return func() (*index.Bucket[[]byte], func(), error) {
		bk, err := b.rlockBucket(ctx, bucketName)
		if err != nil {
			return nil, nil, err
		}
		return bk.ix, bk.mu.RUnlock, nil
	}
}

// WalkObjects calls fn with the latest version of each key (skipping keys
// whose latest version is a delete marker) in ascending key order, for keys
// with the given prefix that are strictly greater than after. fn runs with
// no lock held and may call into the backend.
func (b *Backend) WalkObjects(ctx context.Context, bucketName, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("memory: nil walk function")
	}
	return index.WalkObjects(ctx, b.walkAcquire(ctx, bucketName), prefix, after, fn)
}

// WalkVersions calls fn for every version and delete marker of keys with the
// given prefix, keys ascending and newest version first within a key,
// starting after (keyMarker, versionIDMarker) per S3 ListObjectVersions
// semantics. If versionIDMarker is not a version of keyMarker, the walk
// resumes at the position the ID would occupy when it is an ID generated by
// this backend (e.g. a version deleted between pages), with every version of
// keyMarker when it is "null" (a deleted null version has no position), and
// otherwise after all versions of keyMarker. fn runs with no lock held.
func (b *Backend) WalkVersions(ctx context.Context, bucketName, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("memory: nil walk function")
	}
	return index.WalkVersions(ctx, b.walkAcquire(ctx, bucketName), prefix, keyMarker, versionIDMarker, fn)
}
