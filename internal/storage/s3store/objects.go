package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/index"
)

// errWriterDone is returned when writing to or committing a writer that was
// already committed or aborted.
var errWriterDone = errors.New("s3store: writer already committed or aborted")

// checkCompression validates c before any data is written.
func checkCompression(c storage.Compression) error {
	switch c.Algorithm {
	case "":
		return nil
	case storage.CompressionDeflate:
		if !compress.ValidLevel(c.Level) {
			return fmt.Errorf("s3store: invalid deflate level %d", c.Level)
		}
		return nil
	default:
		return fmt.Errorf("s3store: unsupported compression algorithm %q", c.Algorithm)
	}
}

// checkWriteOptions refuses what the backend cannot store. Encryption
// needs a key-encryption key (and restic the backing's credentials); a
// write asking for it otherwise fails, never falling back to plaintext.
func (b *Backend) checkWriteOptions(o storage.WriteOptions) error {
	switch o.Encryption {
	case "":
	case storage.EncryptionAES256, storage.EncryptionRestic:
		if len(b.conn.Load().kek) == 0 {
			return b.errNoKEK(fmt.Errorf("encryption %q: %w", o.Encryption, errors.ErrUnsupported))
		}
		if o.Encryption == storage.EncryptionRestic {
			if _, ok := b.resticRepository("x"); !ok {
				return fmt.Errorf("s3store: restic needs the backing's access key: %w", errors.ErrUnsupported)
			}
		}
	default:
		return fmt.Errorf("s3store: encryption %q: %w", o.Encryption, errors.ErrUnsupported)
	}
	return checkCompression(o.Compression)
}

// --- data writers ---

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// dataWriter spools the data of a new version or part, compressed and
// encrypted on request (compress, then crypt.Writer under the key of the
// blob's data ID), and uploads it as a blob.
type dataWriter struct {
	b  *Backend
	mu sync.Mutex
	sp *s3client.Spool
	// w is where written data goes: zw, or encoded.
	w       io.Writer
	zw      *compress.Writer
	cw      *crypt.Writer
	encoded *countingWriter // the compressed (or raw) bytes, before encryption
	// id is the blob's data ID (fixed up front: it keys the encryption).
	id          string
	compression string
	encrypted   bool
	size        int64
	done        bool
}

// newDataWriter returns a writer compressing with c and, with a master
// key, encrypting.
func (b *Backend) newDataWriter(c storage.Compression, master []byte) (*dataWriter, error) {
	w := &dataWriter{b: b, sp: s3client.NewSpool(b.opts.TempDir, 0), id: randomID(16)}
	var sink io.Writer = w.sp
	if master != nil {
		cw, err := crypt.NewWriter(w.sp, crypt.ObjectDataKey(master, w.id))
		if err != nil {
			_ = w.sp.Close()
			return nil, err
		}
		w.cw, w.encrypted, sink = cw, true, cw
	}
	w.encoded = &countingWriter{w: sink}
	w.w = w.encoded
	if c.Enabled() {
		zw, err := compress.NewWriter(w.encoded, c.Level)
		if err != nil {
			_ = w.sp.Close()
			return nil, err
		}
		w.zw, w.compression, w.w = zw, c.Algorithm, zw
	}
	return w, nil
}

func (w *dataWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	n, err := w.w.Write(p)
	w.size += int64(n)
	return n, err
}

// close ends the stream and returns the content size; the writer is done.
func (w *dataWriter) close() (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	w.done = true
	var err error
	if w.zw != nil {
		err = w.zw.Close()
	}
	if err == nil && w.cw != nil {
		err = w.cw.Close()
	}
	if err == nil {
		err = w.sp.Finish()
	}
	if err != nil {
		_ = w.sp.Close()
		return 0, err
	}
	return w.size, nil
}

// blob returns the blob reference of the closed stream.
func (w *dataWriter) blob() blobRef {
	r := blobRef{ID: w.id, Size: w.sp.Size()}
	if w.encrypted {
		r.Enc = storage.EncryptionAES256
	}
	return r
}

// upload stores the closed stream as the data blob of bucket bucketID and
// returns the content and the blob key ("" for empty content), which is
// pending until the caller calls b.pending.done.
func (w *dataWriter) upload(ctx context.Context, bucketID string) (index.Content[blobRef], string, error) {
	defer w.sp.Close()
	if w.size == 0 {
		return index.Content[blobRef]{}, "", nil
	}
	b := w.b
	key := b.dataKey(bucketID, w.id)
	ref := w.blob()
	ref.Enc = "" // versions record their encoding in their metadata
	b.pending.add(key)
	if err := b.putBlob(ctx, key, w.sp, ref.Size); err != nil {
		b.pending.done(key)
		return index.Content[blobRef]{}, "", err
	}
	c := index.Content[blobRef]{Data: ref, Size: w.size}
	if w.compression != "" {
		c.Compression, c.StoredSize = w.compression, w.encoded.n
	}
	return c, key, nil
}

func (w *dataWriter) abort() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done {
		if w.zw != nil {
			w.zw.Abandon()
		}
		_ = w.sp.Close()
	}
	w.done = true
}

// contentWriter writes the data of a new version as the write options ask:
// into a blob (plain or AES256) or into a restic snapshot.
type contentWriter struct {
	b          *Backend
	bk         *bucket
	bucket     string
	key        string
	encryption string
	dw         *dataWriter
	rw         *resticWriter
	mu         sync.Mutex // serializes rw
	done       bool
}

// newContentWriter checks the options and starts the data of a new
// version of bucket/key.
func (b *Backend) newContentWriter(ctx context.Context, bucket, key string, opts storage.WriteOptions) (*contentWriter, error) {
	if err := b.checkWriteOptions(opts); err != nil {
		return nil, err
	}
	if err := index.ValidKey(key); err != nil {
		return nil, err
	}
	bk, err := b.rlockBucket(ctx, bucket)
	if err != nil {
		return nil, err
	}
	bk.mu.RUnlock()
	w := &contentWriter{b: b, bk: bk, bucket: bucket, key: key, encryption: opts.Encryption}
	switch opts.Encryption {
	case storage.EncryptionRestic:
		// restic compresses itself.
		w.rw, err = b.newResticWriter(ctx, bk, bucket, key)
	case storage.EncryptionAES256:
		var master []byte
		if master, err = b.bucketKey(ctx, bk, true); err == nil {
			w.dw, err = b.newDataWriter(opts.Compression, master)
		}
	default:
		w.dw, err = b.newDataWriter(opts.Compression, nil)
	}
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (w *contentWriter) Write(p []byte) (int, error) {
	if w.dw != nil {
		return w.dw.Write(p)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	return w.rw.Write(p)
}

// finish ends the data and stores it: check (given the content size) runs
// before anything is stored, so a refused restic write saves no snapshot.
// It returns the content and the pending blob key ("" if none).
func (w *contentWriter) finish(ctx context.Context, check func(size int64) error) (index.Content[blobRef], string, error) {
	if w.dw != nil {
		size, err := w.dw.close()
		if err != nil {
			return index.Content[blobRef]{}, "", err
		}
		if check != nil {
			if err := check(size); err != nil {
				_ = w.dw.sp.Close()
				return index.Content[blobRef]{}, "", err
			}
		}
		return w.dw.upload(ctx, w.bk.id)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return index.Content[blobRef]{}, "", errWriterDone
	}
	w.done = true
	snap, err := w.rw.a.Finish(func() error {
		if check != nil {
			return check(w.rw.size)
		}
		return nil
	})
	if err != nil {
		return index.Content[blobRef]{}, "", err
	}
	return index.Content[blobRef]{Data: blobRef{Restic: snap}, Size: w.rw.size}, "", nil
}

func (w *contentWriter) abort() {
	if w.dw != nil {
		w.dw.abort()
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	w.rw.a.Abort()
}

// checkPreconditions evaluates the commit preconditions against bk early.
func (b *Backend) checkPreconditions(bk *bucket, key string, opts storage.CommitOptions) error {
	if !opts.IfNoneMatch && opts.IfMatch == "" {
		return nil
	}
	bk.mu.RLock()
	defer bk.mu.RUnlock()
	if bk.gone() {
		return b.goneErr(bk)
	}
	return bk.ix.CheckPreconditions(key, opts)
}

// --- object writer ---

type objectWriter struct {
	*contentWriter
}

// NewObjectWriter starts writing a new version of bucket/key.
func (b *Backend) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	return b.NewObjectWriterWith(ctx, bucket, key, storage.WriteOptions{})
}

// NewObjectWriterWith is NewObjectWriter with write options (see
// storage.CompressingBackend): compression, and encryption with
// storage.EncryptionAES256 (compress, then encrypt) or
// storage.EncryptionRestic (archived while written; not compressed by
// gos3).
func (b *Backend) NewObjectWriterWith(ctx context.Context, bucket, key string, opts storage.WriteOptions) (storage.ObjectWriter, error) {
	w, err := b.newContentWriter(ctx, bucket, key, opts)
	if err != nil {
		return nil, err
	}
	return &objectWriter{w}, nil
}

func (w *objectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	b, bk := w.b, w.bk
	c, key, err := w.finish(ctx, func(int64) error { return b.checkPreconditions(bk, w.key, opts) })
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if key != "" {
		defer b.pending.done(key)
	}
	if err := b.relock(ctx, bk); err != nil {
		b.deleteLater(key)
		return storage.ObjectInfo{}, err
	}
	meta.Encryption = w.encryption
	info, ch, err := bk.ix.Commit(w.key, c, meta, opts)
	if err != nil {
		bk.mu.Unlock()
		b.deleteLater(key)
		return storage.ObjectInfo{}, err
	}
	done := b.appendChange(bk, ch, "")
	bk.mu.Unlock()
	if err := await(ctx, done); err != nil {
		return storage.ObjectInfo{}, err
	}
	return info, nil
}

func (w *objectWriter) Abort() error {
	w.abort()
	return nil
}

// appendChange journals an index change of bk (write-locked) and, with
// rmUpload, the removal of that upload; the blobs of removed versions (and
// extra) are deleted once the record is durable.
func (b *Backend) appendChange(bk *bucket, ch index.Change[blobRef], rmUpload string, extra ...string) <-chan error {
	drop := append(b.blobsOf(bk.id, ch.Removed), extra...)
	onDurable := b.dropOnDurable(drop)
	return b.appendLocked(bk, &record{Op: opChange, B: bk.ix.Info.Name, C: encodeChange(ch), RmUpload: rmUpload}, onDurable)
}

// --- reads ---

// GetObjectInfo returns metadata for a version ("" = latest, which may be
// a delete marker).
func (b *Backend) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	bk, err := b.rlockBucket(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	v, latest, err := bk.ix.Find(key, versionID)
	lsn := bk.lsn
	var info storage.ObjectInfo
	if err == nil {
		info = v.Info(bk.ix.Info.Name, key, latest)
	}
	bk.mu.RUnlock()
	if werr := b.waitRead(ctx, lsn); werr != nil {
		return storage.ObjectInfo{}, werr
	}
	return info, err
}

// OpenObject returns a version's info and a reader over its content (nil
// for delete markers). The reader keeps reading that version's blob after
// the version is overwritten or deleted: the blob's delete waits for it.
func (b *Backend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	bk, err := b.rlockBucket(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	v, latest, err := bk.ix.Find(key, versionID)
	lsn := bk.lsn
	if err != nil {
		bk.mu.RUnlock()
		if werr := b.waitRead(ctx, lsn); werr != nil {
			return storage.ObjectInfo{}, nil, werr
		}
		return storage.ObjectInfo{}, nil, err
	}
	info := v.Info(bk.ix.Info.Name, key, latest)
	data := v.Data
	var blobKey string
	if !v.DeleteMarker && data.ID != "" {
		blobKey = b.dataKey(bk.id, data.ID)
		b.blobs.acquire(blobKey)
	}
	bk.mu.RUnlock()
	if err := b.waitRead(ctx, lsn); err != nil {
		if blobKey != "" {
			b.releaseBlob(blobKey)
		}
		return storage.ObjectInfo{}, nil, err
	}
	if v.DeleteMarker {
		return info, nil, nil
	}
	r, err := b.openContent(ctx, bk, info, data, blobKey)
	if err != nil {
		return storage.ObjectInfo{}, nil, fmt.Errorf("s3store: open %s/%s: %w", bucket, key, err)
	}
	return info, r, nil
}

// openContent returns the reader of a version's content stored as data;
// blobKey (acquired by the caller) is released on failure or by the
// reader's Close.
func (b *Backend) openContent(ctx context.Context, bk *bucket, info storage.ObjectInfo, data blobRef, blobKey string) (io.ReadSeekCloser, error) {
	if info.Encryption == storage.EncryptionRestic {
		if data.Restic == "" {
			return emptyReader{bytes.NewReader(nil)}, nil
		}
		loc, err := b.resticLocation(ctx, bk)
		if err != nil {
			return nil, err
		}
		return b.restic.Open(ctx, loc, data.Restic, info.Size)
	}
	if blobKey == "" {
		return emptyReader{bytes.NewReader(nil)}, nil
	}
	encoded := info.Size
	if info.Compression != "" {
		encoded = info.StoredSize
	}
	var master []byte
	if info.Encryption == storage.EncryptionAES256 {
		var err error
		if master, err = b.bucketKey(ctx, bk, false); err != nil {
			b.releaseBlob(blobKey)
			return nil, err
		}
		if want := storage.EncryptedSize(encoded); data.Size != want {
			b.releaseBlob(blobKey)
			return nil, fmt.Errorf("%w: encrypted blob %s has %d bytes, expected %d", errBlobMissing, blobKey, data.Size, want)
		}
	}
	br := b.newBlobReader(ctx, blobKey, data.Size, true, missingData(blobKey))
	var stream interface {
		io.ReadSeeker
		io.ReaderAt
	} = br
	if master != nil {
		cr, err := crypt.NewReader(br, crypt.ObjectDataKey(master, data.ID), encoded)
		if err != nil {
			_ = br.Close()
			return nil, err
		}
		stream = cr
	}
	if info.Compression == "" {
		if master == nil {
			return br, nil
		}
		return &closeBoth{ReadSeeker: stream, closers: []io.Closer{br}}, nil
	}
	z, err := compress.NewReader(stream, info.StoredSize)
	if err != nil {
		_ = br.Close()
		return nil, err
	}
	return &closeBoth{ReadSeeker: z, closers: []io.Closer{z, br}}, nil
}

// --- metadata ---

// UpdateObjectMeta atomically mutates the metadata of an existing version.
func (b *Backend) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	return b.updateObjectMeta(ctx, bucket, key, versionID, nil, fn)
}

// UpdateObjectMetaIf is UpdateObjectMeta made conditional on check (see
// storage.ConditionalMetaUpdater).
func (b *Backend) UpdateObjectMetaIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if check == nil {
		return storage.ObjectInfo{}, errors.New("s3store: nil update check")
	}
	return b.updateObjectMeta(ctx, bucket, key, versionID, check, fn)
}

func (b *Backend) updateObjectMeta(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if fn == nil {
		return storage.ObjectInfo{}, errors.New("s3store: nil update function")
	}
	bk, err := b.lockBucket(ctx, bucket)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	info, ch, err := bk.ix.UpdateMeta(key, versionID, check, fn)
	if err != nil {
		lsn := bk.lsn
		bk.mu.Unlock()
		if werr := b.waitRead(ctx, lsn); werr != nil {
			return storage.ObjectInfo{}, werr
		}
		return storage.ObjectInfo{}, err
	}
	done := b.appendChange(bk, ch, "")
	bk.mu.Unlock()
	if err := await(ctx, done); err != nil {
		return storage.ObjectInfo{}, err
	}
	return info, nil
}

// --- deletes ---

// DeleteObject deletes a version or creates a delete marker per S3
// semantics (see storage.Backend).
func (b *Backend) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	return b.deleteObject(ctx, bucket, func(ix *index.Bucket[blobRef]) (storage.DeleteResult, index.Change[blobRef], error) {
		return ix.Delete(key, versionID)
	})
}

// DeleteObjectIf is DeleteObject made conditional on check (see
// storage.ConditionalDeleter).
func (b *Backend) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("s3store: nil delete check")
	}
	return b.deleteObject(ctx, bucket, func(ix *index.Bucket[blobRef]) (storage.DeleteResult, index.Change[blobRef], error) {
		return ix.DeleteIf(key, versionID, func(o storage.ObjectInfo, _ bool) error { return check(o) }, false)
	})
}

// DeleteVersionIf permanently deletes a version when check accepts it and
// the versions newer than it (see storage.NewerVersionsDeleter).
func (b *Backend) DeleteVersionIf(ctx context.Context, bucket, key, versionID string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("s3store: nil delete check")
	}
	return b.deleteObject(ctx, bucket, func(ix *index.Bucket[blobRef]) (storage.DeleteResult, index.Change[blobRef], error) {
		return ix.DeleteVersionIf(key, versionID, limit, check)
	})
}

// DeleteObjectChecked is DeleteObjectIf whose check is also consulted when
// the key has no versions (see storage.CheckedDeleter).
func (b *Backend) DeleteObjectChecked(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo, bool) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("s3store: nil delete check")
	}
	return b.deleteObject(ctx, bucket, func(ix *index.Bucket[blobRef]) (storage.DeleteResult, index.Change[blobRef], error) {
		return ix.DeleteIf(key, versionID, check, true)
	})
}

func (b *Backend) deleteObject(ctx context.Context, bucket string, op func(*index.Bucket[blobRef]) (storage.DeleteResult, index.Change[blobRef], error)) (storage.DeleteResult, error) {
	bk, err := b.lockBucket(ctx, bucket)
	if err != nil {
		return storage.DeleteResult{}, err
	}
	res, ch, err := op(bk.ix)
	if err != nil || ch.Empty() {
		lsn := bk.lsn
		bk.mu.Unlock()
		if werr := b.waitRead(ctx, lsn); werr != nil {
			return storage.DeleteResult{}, werr
		}
		return res, err
	}
	done := b.appendChange(bk, ch, "")
	bk.mu.Unlock()
	if err := await(ctx, done); err != nil {
		return storage.DeleteResult{}, err
	}
	return res, nil
}

// --- walks ---

// walkAcquire returns the acquire function of the index walk helpers.
func (b *Backend) walkAcquire(ctx context.Context, bucket string) func() (*index.Bucket[blobRef], func(), error) {
	return func() (*index.Bucket[blobRef], func(), error) {
		bk, err := b.rlockBucket(ctx, bucket)
		if err != nil {
			return nil, nil, err
		}
		return bk.ix, bk.mu.RUnlock, nil
	}
}

// WalkObjects calls fn with the latest version of each key that is not a
// delete marker (see storage.Backend). fn runs with no lock held.
func (b *Backend) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("s3store: nil walk function")
	}
	return index.WalkObjects(ctx, b.walkAcquire(ctx, bucket), prefix, after, fn)
}

// WalkVersions calls fn for every version and delete marker (see
// storage.Backend). fn runs with no lock held.
func (b *Backend) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	if fn == nil {
		return errors.New("s3store: nil walk function")
	}
	return index.WalkVersions(ctx, b.walkAcquire(ctx, bucket), prefix, keyMarker, versionIDMarker, fn)
}
