package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"sync"
	"time"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
)

// errWriterDone is returned when writing to or committing a writer that was
// already committed or aborted.
var errWriterDone = errors.New("fs: writer already committed or aborted")

// --- commit ---

// commitReq describes a new object version whose data file or blob (if
// any) is already in its final place.
type commitReq struct {
	bucket   string
	bucketID string
	key      string
	dataID   string // "" for empty objects
	size     int64
	// compression and storedSize describe a compressed data file
	// (versionRecord.Compression, StoredSize).
	compression string
	storedSize  int64
	meta        storage.ObjectMeta
	opts        storage.CommitOptions
	// pre runs first inside the transaction (multipart: verify and remove
	// the upload). Its error aborts the commit.
	pre func(tx *bbolt.Tx, bb *bbolt.Bucket) error
	// imported, when set, stores the version under the identity it has on
	// another backend (import.go) instead of assigning a new one; opts is
	// then ignored.
	imported *importedIdentity
	// rem is the remote holding the data blob (nil: a local data file).
	rem *remote
}

// commitObject publishes a new version in one write transaction, evaluating
// preconditions atomically. On failure the request's data file or blob is
// removed (unless the commit itself failed, in which case it may have
// persisted anyway, see updateFiles); on success the data of replaced
// versions is unlinked (remote blobs: deleted, or once their last reader
// closes).
func (b *Backend) commitObject(req commitReq) (storage.ObjectInfo, error) {
	var (
		info   storage.ObjectInfo
		unlink []string
	)
	indeterminate, err := b.updateFiles(func(tx *bbolt.Tx) error {
		unlink = nil
		br, bb, err := loadBucketID(tx, req.bucket, req.bucketID)
		if err != nil {
			return err
		}
		if req.pre != nil {
			if err := req.pre(tx, bb); err != nil {
				return err
			}
		}
		rec := versionRecord{
			Size:        req.size,
			DataID:      req.dataID,
			Meta:        req.meta,
			Compression: req.compression,
			StoredSize:  req.storedSize,
		}
		if req.imported != nil {
			rec.VersionID = req.imported.versionID
			rec.LastModified = req.imported.lastModified
			rec.DeleteMarker = req.imported.deleteMarker
			unlink, err = importVersion(bb, req.key, &rec)
		} else {
			if err := checkPreconditions(bb, req.key, req.opts); err != nil {
				return err
			}
			created := time.Now().UTC()
			if req.opts.BeforePublish != nil {
				latest, _, err := latestVersion(bb, req.key)
				if err != nil {
					return err
				}
				var current *storage.ObjectMeta
				if latest != nil && !latest.DeleteMarker {
					m := cloneMeta(latest.Meta)
					current = &m
				}
				config := bb.Bucket(subConfig)
				if err := req.opts.BeforePublish(&req.meta, current, created, func(key string) []byte {
					return config.Get([]byte(key))
				}); err != nil {
					return err
				}
				req.meta.Encryption = rec.Meta.Encryption
				rec.Meta = req.meta
			}
			rec.LastModified = created
			unlink, err = addVersion(br, bb, req.key, &rec)
		}
		if err != nil {
			return err
		}
		info = rec.info(req.bucket, req.key, true)
		return nil
	})
	if err != nil {
		if req.dataID != "" {
			switch {
			case indeterminate:
				b.log.Warn("fs: metadata commit failed after the transaction was written; keeping its data file",
					"bucket_id", req.bucketID, "data_id", req.dataID, "err", err)
			case req.rem != nil:
				b.removeBlob(req.rem, req.rem.dataKey(req.bucketID, req.dataID))
			default:
				_ = os.Remove(b.dataPath(req.bucketID, req.dataID))
			}
		}
		return storage.ObjectInfo{}, err
	}
	if req.rem != nil {
		for _, id := range unlink {
			if validDataID(id) {
				b.dropBlob(req.rem, req.rem.dataKey(req.bucketID, id))
			}
		}
	} else {
		b.unlinkData(req.bucketID, unlink)
	}
	// The returned meta must not alias anything the caller passed in.
	return cloneInfo(info), nil
}

// finishData closes t and moves it to its final data path, or, for a bucket
// on the remote blob backing rem, uploads it as the data blob. It returns
// the data ID ("" for empty files, which are discarded) and registers the
// final path (remote: the blob's ID) as pending; the caller must call
// b.pending.done(path) after the metadata transaction finished.
func (b *Backend) finishData(ctx context.Context, t *tmpFile, bucketID string, rem *remote) (dataID, path string, err error) {
	if err := t.finish(b.fsync && rem == nil, false); err != nil {
		return "", "", err
	}
	if t.size == 0 {
		t.remove()
		return "", "", nil
	}
	dataID = t.cryptoID
	if dataID == "" {
		dataID = randomHex(16)
	}
	if rem != nil {
		key := rem.dataKey(bucketID, dataID)
		path = rem.blobID(key)
		b.pending.add(path)
		if err := b.uploadTemp(ctx, rem, key, t); err != nil {
			b.pending.done(path)
			return "", "", err
		}
		return dataID, path, nil
	}
	path = b.dataPath(bucketID, dataID)
	b.pending.add(path)
	if err := b.place(t.path, path); err != nil {
		b.pending.done(path)
		t.remove()
		return "", "", err
	}
	return dataID, path, nil
}

// --- compression ---

// The optional storage interface implemented by NewObjectWriterWith and
// CompleteMultipartUploadWith.
var _ storage.CompressingBackend = (*Backend)(nil)

// checkCompression validates c before any data is written.
func checkCompression(c storage.Compression) error {
	switch c.Algorithm {
	case "":
		return nil
	case storage.CompressionDeflate:
		if !compress.ValidLevel(c.Level) {
			return fmt.Errorf("fs: invalid deflate level %d", c.Level)
		}
		return nil
	default:
		return fmt.Errorf("fs: unsupported compression algorithm %q", c.Algorithm)
	}
}

// newCompressor returns a writer compressing into t as c asks (checked
// with checkCompression), or nil when c asks for no compression.
func newCompressor(t *tmpFile, c storage.Compression) (*compress.Writer, error) {
	if !c.Enabled() {
		return nil, nil
	}
	return compress.NewWriter(t, c.Level)
}

// sealData finishes the compressor cw (if any) writing into t and returns
// the content size and the compression fields of the version record.
// Empty content is stored as an empty, uncompressed object.
func sealData(t *tmpFile, cw *compress.Writer, algorithm string) (size int64, compression string, stored int64, err error) {
	if cw == nil {
		return t.size, "", 0, nil
	}
	if err := cw.Close(); err != nil {
		return 0, "", 0, err
	}
	if cw.Size() == 0 {
		return 0, "", 0, nil
	}
	return cw.Size(), algorithm, t.size, nil
}

// --- object writer ---

type objectWriter struct {
	b        *Backend
	bucket   string
	bucketID string
	key      string
	rem      *remote // remote blob backing of the bucket (nil: local)

	mu sync.Mutex
	t  *tmpFile
	// cw compresses into t when the version is stored compressed; rw
	// archives restic versions instead of t; w is where written data goes
	// (cw, rw or t).
	cw          *compress.Writer
	rw          *resticWriter
	w           io.Writer
	compression string
	encryption  string
	done        bool
}

// NewObjectWriter starts writing a new version of bucket/key. Data is
// streamed to a temporary file (uploaded as a blob at Commit for a bucket
// on a remote blob backing); nothing is visible until Commit.
func (b *Backend) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	return b.NewObjectWriterWith(ctx, bucket, key, storage.WriteOptions{})
}

// NewObjectWriterWith is NewObjectWriter with write options: with
// opts.Compression the data file is written in the compress package's
// format (see storage.CompressingBackend). With opts.Encryption restic the
// data streams into restic as it is written (see resticWriter) and is not
// compressed by gos3.
func (b *Backend) NewObjectWriterWith(ctx context.Context, bucket, key string, opts storage.WriteOptions) (storage.ObjectWriter, error) {
	if err := checkCompression(opts.Compression); err != nil {
		return nil, err
	}
	if err := validKey(key); err != nil {
		return nil, err
	}
	id, rem, err := b.bucketPlacement(ctx, bucket)
	if err != nil {
		return nil, err
	}
	w := &objectWriter{b: b, bucket: bucket, bucketID: id, key: key, rem: rem, encryption: opts.Encryption}
	if opts.Encryption == storage.EncryptionRestic {
		if w.rw, err = b.newResticWriter(ctx, id, bucket, key); err != nil {
			return nil, err
		}
		w.rw.rem = rem
		w.t, w.w = b.newTemp(), w.rw
		return w, nil
	}
	if w.t, err = b.encryptedTemp(id, opts.Encryption); err != nil {
		return nil, err
	}
	w.compression = opts.Compression.Algorithm
	if w.cw, err = newCompressor(w.t, opts.Compression); err != nil {
		return nil, err
	}
	w.w = w.t
	if w.cw != nil {
		w.w = w.cw
	}
	return w, nil
}

func (w *objectWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	return w.w.Write(p)
}

// discard drops the written data.
func (w *objectWriter) discard() {
	if w.cw != nil {
		w.cw.Abandon()
	}
	if w.rw != nil {
		w.rw.abort()
	}
	w.t.discard()
}

func (w *objectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return storage.ObjectInfo{}, errWriterDone
	}
	w.done = true
	defer w.discard()
	if err := w.b.begin(ctx); err != nil {
		return storage.ObjectInfo{}, err
	}
	var (
		size, stored int64
		compression  string
		dataID, path string
		err          error
	)
	if w.rw != nil {
		// A write refused by its preconditions must not leave a snapshot.
		size = w.rw.size
		dataID, path, err = w.rw.finish(func() error {
			return w.b.checkWriteConditions(w.bucket, w.bucketID, w.key, opts)
		})
	} else {
		size, compression, stored, err = sealData(w.t, w.cw, w.compression)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		dataID, path, err = w.b.finishData(ctx, w.t, w.bucketID, w.rem)
	}
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if path != "" {
		defer w.b.pending.done(path)
	}
	meta.Encryption = w.encryption
	return w.b.commitObject(commitReq{
		rem:         w.rem,
		bucket:      w.bucket,
		bucketID:    w.bucketID,
		key:         w.key,
		dataID:      dataID,
		size:        size,
		compression: compression,
		storedSize:  stored,
		meta:        cloneMeta(meta),
		opts:        opts,
	})
}

func (w *objectWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return nil
	}
	w.done = true
	w.discard()
	return nil
}

// --- reads ---

// objectLoc locates the data of a version.
type objectLoc struct {
	bucketID, dataID, backing string
}

// lookupObject returns the info of a version plus the bucket ID and data ID.
func (b *Backend) lookupObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, string, string, error) {
	info, loc, err := b.lookupObjectLoc(ctx, bucket, key, versionID)
	return info, loc.bucketID, loc.dataID, err
}

// lookupObjectLoc returns the info of a version plus the location of its
// data.
func (b *Backend) lookupObjectLoc(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, objectLoc, error) {
	if err := b.begin(ctx); err != nil {
		return storage.ObjectInfo{}, objectLoc{}, err
	}
	var (
		info storage.ObjectInfo
		loc  objectLoc
	)
	err := b.view(func(tx *bbolt.Tx) error {
		br, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		rec, _, latest, err := findVersion(bb, bucket, key, versionID)
		if err != nil {
			return err
		}
		info = rec.info(bucket, key, latest)
		loc = objectLoc{bucketID: br.ID, dataID: rec.DataID, backing: br.Backing}
		return nil
	})
	return info, loc, err
}

// GetObjectInfo returns metadata for a version ("" = latest, which may be a
// delete marker). A missing key yields ErrObjectNotFound when versionID is
// "" and ErrVersionNotFound otherwise.
func (b *Backend) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	info, _, _, err := b.lookupObject(ctx, bucket, key, versionID)
	return info, err
}

// openRetries bounds how often OpenObject re-resolves a version whose data
// file vanished between the metadata lookup and the open (concurrent
// overwrite or delete).
const openRetries = 8

// OpenObject returns a version's info and a reader over its content (an
// *os.File for non-empty objects, or a ranged-GET reader over the blob of
// a remote backing, below the decryption and decompression layers). The
// reader is nil for delete markers.
func (b *Backend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	for attempt := 0; ; attempt++ {
		info, loc, err := b.lookupObjectLoc(ctx, bucket, key, versionID)
		bucketID, dataID := loc.bucketID, loc.dataID
		if err != nil {
			return storage.ObjectInfo{}, nil, err
		}
		if info.DeleteMarker {
			return info, nil, nil
		}
		if dataID == "" {
			if info.Size != 0 {
				return storage.ObjectInfo{}, nil, fmt.Errorf("%w: %s/%s has size %d but no data file", errCorrupt, bucket, key, info.Size)
			}
			return info, newEmptyReader(), nil
		}
		if !validDataID(dataID) {
			return storage.ObjectInfo{}, nil, fmt.Errorf("%w: invalid data ID for %s/%s", errCorrupt, bucket, key)
		}
		if loc.backing != "" {
			r, retry, err := b.openRemote(ctx, bucket, info, loc)
			if err != nil {
				return storage.ObjectInfo{}, nil, fmt.Errorf("fs: open data of %s/%s: %w", bucket, key, err)
			}
			if retry {
				if attempt < openRetries {
					continue
				}
				return storage.ObjectInfo{}, nil, fmt.Errorf("fs: open data of %s/%s: too much contention", bucket, key)
			}
			return info, r, nil
		}
		f, err := openShared(b.dataPath(bucketID, dataID))
		if err != nil {
			if (errors.Is(err, iofs.ErrNotExist) || openRaced(err)) && attempt < openRetries {
				// Superseded and unlinked after our lookup (or, on Windows,
				// being unlinked right now); the metadata has changed, so
				// resolve again.
				continue
			}
			return storage.ObjectInfo{}, nil, fmt.Errorf("fs: open data of %s/%s: %w", bucket, key, err)
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return storage.ObjectInfo{}, nil, fmt.Errorf("fs: stat data of %s/%s: %w", bucket, key, err)
		}
		want := info.PhysicalSize()
		if info.Encryption == storage.EncryptionRestic {
			// The data file is the snapshot reference, whatever the
			// version's size accounting says.
			want = snapshotRefSize
		}
		if st.Size() != want {
			_ = f.Close()
			return storage.ObjectInfo{}, nil, fmt.Errorf("%w: data file of %s/%s has %d bytes, expected %d", errCorrupt, bucket, key, st.Size(), want)
		}
		if info.Encryption == storage.EncryptionRestic {
			rd, err := b.openRestic(ctx, f, bucketID, info.Size)
			return info, rd, err
		}
		// data reads the stored bytes: the file, or its decrypted stream.
		var data dataReader = &fileReader{f: f}
		if info.Encryption == storage.EncryptionAES256 {
			encoded := info.Size
			if info.Compression != "" {
				encoded = info.StoredSize
			}
			if data, err = b.openEncrypted(f, bucketID, dataID, encoded); err != nil {
				_ = f.Close()
				return storage.ObjectInfo{}, nil, fmt.Errorf("fs: open data of %s/%s: %w", bucket, key, err)
			}
		}
		if info.Compression == "" {
			return info, data, nil
		}
		rd, err := openCompressed(data, info)
		if err != nil {
			_ = data.Close()
			return storage.ObjectInfo{}, nil, fmt.Errorf("fs: open data of %s/%s: %w", bucket, key, err)
		}
		return info, rd, nil
	}
}

// openRemote returns a reader over the content of the version info whose
// data is the remote blob of loc. The blob is registered as open before
// the metadata is checked again, so a concurrent overwrite or delete is
// either seen by the check (retry reports true: resolve the version again)
// or sees the reader and defers deleting the blob until it is closed.
func (b *Backend) openRemote(ctx context.Context, bucket string, info storage.ObjectInfo, loc objectLoc) (r io.ReadSeekCloser, retry bool, err error) {
	rem, err := b.remoteFor(loc.backing)
	if err != nil {
		return nil, false, err
	}
	key := rem.dataKey(loc.bucketID, loc.dataID)
	b.blobs.acquire(rem.blobID(key))
	var still bool
	err = b.view(func(tx *bbolt.Tx) error {
		_, bb, err := loadBucketID(tx, bucket, loc.bucketID)
		if err != nil {
			return err
		}
		still = bb.Bucket(subData).Get([]byte(loc.dataID)) != nil
		return nil
	})
	if err != nil || !still {
		b.releaseBlob(rem, key)
		return nil, err == nil, err
	}
	stored := info.PhysicalSize()
	if info.Encryption == storage.EncryptionRestic {
		stored = snapshotRefSize
	}
	br := b.newBlobReader(ctx, rem, key, stored, true, rem.corruptBlob(key))
	if info.Encryption == storage.EncryptionRestic {
		// The blob is the snapshot reference; the content is in restic.
		defer br.Close()
		rd, err := b.openResticRef(ctx, br, loc.bucketID, info.Size)
		return rd, false, err
	}
	var data dataReader = br
	if info.Encryption == storage.EncryptionAES256 {
		encoded := info.Size
		if info.Compression != "" {
			encoded = info.StoredSize
		}
		if data, err = b.decryptStored(br, loc.bucketID, loc.dataID, encoded, br.Close); err != nil {
			_ = br.Close()
			return nil, false, err
		}
	}
	if info.Compression == "" {
		return data, false, nil
	}
	rd, err := openCompressed(data, info)
	if err != nil {
		_ = data.Close()
		return nil, false, err
	}
	return rd, false, nil
}

// openCompressed returns a reader over the content of the compressed data
// file f of the version info.
func openCompressed(f interface {
	io.ReaderAt
	io.Closer
}, info storage.ObjectInfo) (io.ReadSeekCloser, error) {
	if info.Compression != storage.CompressionDeflate {
		return nil, fmt.Errorf("%w: unknown compression %q", errCorrupt, info.Compression)
	}
	z, err := compress.NewReader(f, info.StoredSize)
	if err != nil {
		return nil, err
	}
	if z.Size() != info.Size {
		_ = z.Close()
		return nil, fmt.Errorf("%w: compressed data holds %d bytes, expected %d", errCorrupt, z.Size(), info.Size)
	}
	return &compressedReader{Reader: z, f: f}, nil
}

// compressedReader is the reader OpenObject returns for compressed data:
// a compress.Reader over the data file. Close closes both, idempotently.
type compressedReader struct {
	*compress.Reader
	f      io.Closer
	closed sync.Once
	err    error
}

func (r *compressedReader) Close() error {
	r.closed.Do(func() {
		_ = r.Reader.Close()
		r.err = r.f.Close()
	})
	return r.err
}

// dataReader reads the stored bytes of a version: a fileReader, or a
// cryptReader for natively encrypted data.
type dataReader interface {
	io.ReadSeekCloser
	io.ReaderAt
}

// fileReader is the reader returned by OpenObject: an *os.File whose Close is
// idempotent. It forwards ReadAt and WriteTo so io.Copy can still use
// sendfile/splice where the destination supports it.
type fileReader struct {
	f      *os.File
	closed sync.Once
	err    error
}

func (r *fileReader) Read(p []byte) (int, error)                { return r.f.Read(p) }
func (r *fileReader) ReadAt(p []byte, off int64) (int, error)   { return r.f.ReadAt(p, off) }
func (r *fileReader) Seek(off int64, whence int) (int64, error) { return r.f.Seek(off, whence) }
func (r *fileReader) WriteTo(w io.Writer) (int64, error)        { return r.f.WriteTo(w) }

// Close closes the file; subsequent calls return the first call's result.
func (r *fileReader) Close() error {
	r.closed.Do(func() { r.err = r.f.Close() })
	return r.err
}

// updateRetries bounds optimistic-concurrency retries of UpdateObjectMeta.
const updateRetries = 64

// UpdateObjectMeta atomically mutates the metadata of an existing version.
// fn runs outside any transaction on a private copy (so it may call back
// into the backend); the result is stored only if the version did not change
// meanwhile, otherwise fn is re-run on the fresh metadata. Addressing a
// delete marker yields ErrObjectNotFound.
func (b *Backend) UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	return b.updateObjectMeta(ctx, bucket, key, versionID, nil, fn)
}

// The optional storage interfaces implemented in this file and buckets.go.
var (
	_ storage.ConditionalMetaUpdater = (*Backend)(nil)
	_ storage.ConfigViewer           = (*Backend)(nil)
	_ storage.CheckedDeleter         = (*Backend)(nil)
)

// UpdateObjectMetaIf is UpdateObjectMeta made conditional on check (see
// storage.ConditionalMetaUpdater). check sees the version as it was read
// for the update; the update is stored only if that version is unchanged
// when it is written (otherwise both run again on the fresh version), so
// the check holds at the time of the write.
func (b *Backend) UpdateObjectMetaIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if check == nil {
		return storage.ObjectInfo{}, errors.New("fs: nil update check")
	}
	return b.updateObjectMeta(ctx, bucket, key, versionID, check, fn)
}

// updateObjectMeta implements UpdateObjectMeta and, with a non-nil check,
// UpdateObjectMetaIf.
func (b *Backend) updateObjectMeta(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	if fn == nil {
		return storage.ObjectInfo{}, errors.New("fs: nil update function")
	}
	for attempt := 0; attempt < updateRetries; attempt++ {
		if err := b.begin(ctx); err != nil {
			return storage.ObjectInfo{}, err
		}
		var (
			bucketID string
			vk, raw  []byte
			rec      *versionRecord
			latest   bool
		)
		err := b.view(func(tx *bbolt.Tx) error {
			br, bb, err := loadBucket(tx, bucket)
			if err != nil {
				return err
			}
			r, k, isLatest, err := findVersion(bb, bucket, key, versionID)
			if err != nil {
				return err
			}
			if r.DeleteMarker {
				return fmt.Errorf("%w: %s/%s version %q is a delete marker", storage.ErrObjectNotFound, bucket, key, r.VersionID)
			}
			bucketID, vk, rec, latest = br.ID, k, r, isLatest
			raw = bytes.Clone(bb.Bucket(subVersions).Get(k))
			return nil
		})
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if check != nil {
			// The record is stored only if it is still raw below, so the
			// version checked is the version updated.
			if err := check(cloneInfo(rec.info(bucket, key, latest))); err != nil {
				return storage.ObjectInfo{}, err
			}
		}
		encryption := rec.Meta.Encryption
		if err := fn(&rec.Meta); err != nil {
			return storage.ObjectInfo{}, err
		}
		rec.Meta.Encryption = encryption
		rec.Meta = cloneMeta(rec.Meta)
		newRaw, err := encodeRecord(rec)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		conflict := false
		var info storage.ObjectInfo
		err = b.update(func(tx *bbolt.Tx) error {
			conflict = false
			_, bb, err := loadBucketID(tx, bucket, bucketID)
			if err != nil {
				return err
			}
			vers := bb.Bucket(subVersions)
			if cur := vers.Get(vk); !bytes.Equal(cur, raw) {
				conflict = true
				return nil
			}
			if err := vers.Put(vk, newRaw); err != nil {
				return err
			}
			if err := refreshCurrent(bb, key); err != nil {
				return err
			}
			_, latestKey, err := latestVersion(bb, key)
			if err != nil {
				return err
			}
			info = rec.info(bucket, key, bytes.Equal(latestKey, vk))
			return nil
		})
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if !conflict {
			return cloneInfo(info), nil
		}
	}
	return storage.ObjectInfo{}, fmt.Errorf("fs: update metadata of %s/%s: too much contention", bucket, key)
}

// DeleteObject deletes a version or creates a delete marker per S3 semantics
// (see storage.Backend). In an Unversioned bucket the result is always
// DeleteResult{} (S3 reports no version information there).
func (b *Backend) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	return b.deleteObject(ctx, bucket, key, versionID, nil, false)
}

// DeleteObjectIf is DeleteObject made conditional on check, which is called
// with the addressed version inside the delete's write transaction (see
// storage.ConditionalDeleter).
func (b *Backend) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("fs: nil delete check")
	}
	return b.deleteObject(ctx, bucket, key, versionID, func(o storage.ObjectInfo, _ bool) error { return check(o) }, false)
}

// DeleteObjectChecked is DeleteObjectIf whose check is also consulted when
// the key has no versions (see storage.CheckedDeleter).
func (b *Backend) DeleteObjectChecked(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo, bool) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("fs: nil delete check")
	}
	return b.deleteObject(ctx, bucket, key, versionID, check, true)
}

// deleteObject implements DeleteObject and, with a non-nil check,
// DeleteObjectIf (absentOK false) and DeleteObjectChecked (absentOK true).
func (b *Backend) deleteObject(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo, bool) error, absentOK bool) (storage.DeleteResult, error) {
	if err := b.begin(ctx); err != nil {
		return storage.DeleteResult{}, err
	}
	var (
		res               storage.DeleteResult
		unlink            []string
		bucketID, backing string
	)
	err := b.update(func(tx *bbolt.Tx) error {
		res, unlink = storage.DeleteResult{}, nil
		br, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		bucketID, backing = br.ID, br.Backing
		if check != nil {
			rec, _, latest, err := findVersion(bb, bucket, key, versionID)
			switch {
			case err == nil:
				err = check(cloneInfo(rec.info(bucket, key, latest)), true)
			case absentOK && versionID == "" && errors.Is(err, storage.ErrObjectNotFound):
				err = check(storage.ObjectInfo{}, false)
			}
			if err != nil {
				return err
			}
		}
		if versionID != "" {
			rec, vk, _, err := findVersion(bb, bucket, key, versionID)
			if err != nil {
				return err
			}
			id, err := removeVersion(bb, key, vk, rec)
			if err != nil {
				return err
			}
			if id != "" {
				unlink = append(unlink, id)
			}
			res = storage.DeleteResult{VersionID: rec.VersionID, DeleteMarker: rec.DeleteMarker}
			return refreshCurrent(bb, key)
		}
		if br.Versioning == storage.VersioningUnversioned {
			if !lookupKeyOK(key) {
				return nil
			}
			vk, ok := versionPosition(bb, key, storage.NullVersionID)
			if !ok {
				return nil
			}
			v := bb.Bucket(subVersions).Get(vk)
			if v == nil {
				return nil
			}
			var rec versionRecord
			if err := decodeRecord(v, &rec); err != nil {
				return err
			}
			id, err := removeVersion(bb, key, vk, &rec)
			if err != nil {
				return err
			}
			if id != "" {
				unlink = append(unlink, id)
			}
			return refreshCurrent(bb, key)
		}
		// Enabled or Suspended: add a delete marker.
		if err := validKey(key); err != nil {
			return err
		}
		rec := versionRecord{DeleteMarker: true, LastModified: time.Now().UTC()}
		unlink, err = addVersion(br, bb, key, &rec)
		if err != nil {
			return err
		}
		res = storage.DeleteResult{VersionID: rec.VersionID, DeleteMarker: true}
		return nil
	})
	if err != nil {
		return storage.DeleteResult{}, err
	}
	b.dropData(backing, bucketID, unlink)
	return res, nil
}
