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

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
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
	meta     storage.ObjectMeta
	opts     storage.CommitOptions
	// pre runs first inside the transaction (multipart: verify and remove
	// the upload). Its error aborts the commit.
	pre func(tx *bbolt.Tx, bb *bbolt.Bucket) error
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
		if err := checkPreconditions(bb, req.key, req.opts); err != nil {
			return err
		}
		rec := versionRecord{
			Size:         req.size,
			LastModified: time.Now().UTC(),
			DataID:       req.dataID,
			Meta:         req.meta,
		}
		unlink, err = addVersion(br, bb, req.key, &rec)
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

// finishData closes t and moves it to its final data path. It returns the
// data ID ("" for empty files, which are discarded) and registers the final
// path as pending; the caller must call b.pending.done(path) after the
// metadata transaction finished.
func (b *Backend) finishData(t *tmpFile, bucketID string) (dataID, path string, err error) {
	if err := t.finish(b.fsync, false); err != nil {
		return "", "", err
	}
	if t.size == 0 {
		t.remove()
		return "", "", nil
	}
	dataID = randomHex(16)
	path = b.dataPath(bucketID, dataID)
	b.pending.add(path)
	if err := b.place(t.path, path); err != nil {
		b.pending.done(path)
		t.remove()
		return "", "", err
	}
	return dataID, path, nil
}

// --- object writer ---

type objectWriter struct {
	b        *Backend
	bucket   string
	bucketID string
	key      string
	rem      *remote // nil: local data file

	mu   sync.Mutex
	t    *tmpFile        // local
	sp   *s3client.Spool // remote
	done bool
}

// NewObjectWriter starts writing a new version of bucket/key. Data is
// streamed to a temporary file (or, for a bucket on a remote blob backing,
// a spool that is uploaded at Commit); nothing is visible until Commit.
func (b *Backend) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if err := b.begin(ctx); err != nil {
		return nil, err
	}
	var id, backing string
	err := b.view(func(tx *bbolt.Tx) error {
		rec, _, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		id, backing = rec.ID, rec.Backing
		return nil
	})
	if err != nil {
		return nil, err
	}
	rem, err := b.remoteFor(backing)
	if err != nil {
		return nil, err
	}
	w := &objectWriter{b: b, bucket: bucket, bucketID: id, key: key, rem: rem}
	if rem != nil {
		w.sp = b.newSpool()
	} else {
		w.t = b.newTemp()
	}
	return w, nil
}

func (w *objectWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	if w.sp != nil {
		return w.sp.Write(p)
	}
	return w.t.Write(p)
}

func (w *objectWriter) Commit(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return storage.ObjectInfo{}, errWriterDone
	}
	w.done = true
	if w.sp != nil {
		return w.commitRemote(ctx, meta, opts)
	}
	if err := w.b.begin(ctx); err != nil {
		w.t.discard()
		return storage.ObjectInfo{}, err
	}
	dataID, path, err := w.b.finishData(w.t, w.bucketID)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if path != "" {
		defer w.b.pending.done(path)
	}
	return w.b.commitObject(commitReq{
		bucket:   w.bucket,
		bucketID: w.bucketID,
		key:      w.key,
		dataID:   dataID,
		size:     w.t.size,
		meta:     cloneMeta(meta),
		opts:     opts,
	})
}

// commitRemote uploads the spooled data to a fresh blob and commits.
func (w *objectWriter) commitRemote(ctx context.Context, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	b := w.b
	defer w.sp.Close()
	if err := b.begin(ctx); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := w.sp.Finish(); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("fs: spool data: %w", err)
	}
	size := w.sp.Size()
	var dataID string
	if size > 0 {
		dataID = randomHex(16)
		key := w.rem.dataKey(w.bucketID, dataID)
		id := w.rem.blobID(key)
		b.pending.add(id)
		defer b.pending.done(id)
		if err := b.putBlob(ctx, w.rem, key, w.sp, size); err != nil {
			return storage.ObjectInfo{}, err
		}
	}
	return b.commitObject(commitReq{
		bucket:   w.bucket,
		bucketID: w.bucketID,
		rem:      w.rem,
		key:      w.key,
		dataID:   dataID,
		size:     size,
		meta:     cloneMeta(meta),
		opts:     opts,
	})
}

func (w *objectWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return nil
	}
	w.done = true
	if w.sp != nil {
		_ = w.sp.Close()
		return nil
	}
	w.t.discard()
	return nil
}

// --- reads ---

// objectLoc locates the data of a version.
type objectLoc struct {
	bucketID, dataID, backing string
}

// lookupObject returns the info of a version plus the location of its data.
func (b *Backend) lookupObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, objectLoc, error) {
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
	info, _, err := b.lookupObject(ctx, bucket, key, versionID)
	return info, err
}

// openRetries bounds how often OpenObject re-resolves a version whose data
// file vanished between the metadata lookup and the open (concurrent
// overwrite or delete).
const openRetries = 8

// OpenObject returns a version's info and a reader over its content (an
// *os.File for non-empty objects, or a ranged-GET reader over the blob of
// a remote backing). The reader is nil for delete markers.
func (b *Backend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	for attempt := 0; ; attempt++ {
		info, loc, err := b.lookupObject(ctx, bucket, key, versionID)
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
			r, retry, err := b.openRemote(ctx, bucket, loc, info.Size)
			if err != nil {
				return storage.ObjectInfo{}, nil, err
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
		if st.Size() != info.Size {
			_ = f.Close()
			return storage.ObjectInfo{}, nil, fmt.Errorf("%w: data file of %s/%s has %d bytes, expected %d", errCorrupt, bucket, key, st.Size(), info.Size)
		}
		return info, &fileReader{f: f}, nil
	}
}

// openRemote returns a reader over the remote blob of loc (size bytes).
// The blob is registered as open before the metadata is checked again, so
// a concurrent overwrite or delete is either seen by the check (retry
// reports true: resolve the version again) or sees the reader and defers
// deleting the blob until the reader is closed.
func (b *Backend) openRemote(ctx context.Context, bucket string, loc objectLoc, size int64) (r io.ReadSeekCloser, retry bool, err error) {
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
	if ctx == nil {
		ctx = context.Background()
	}
	return &blobReader{b: b, rem: rem, key: key, ctx: ctx, r: rem.client.NewReader(ctx, rem.bucket, key, size, "")}, false, nil
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
		)
		err := b.view(func(tx *bbolt.Tx) error {
			br, bb, err := loadBucket(tx, bucket)
			if err != nil {
				return err
			}
			r, k, _, err := findVersion(bb, bucket, key, versionID)
			if err != nil {
				return err
			}
			if r.DeleteMarker {
				return fmt.Errorf("%w: %s/%s version %q is a delete marker", storage.ErrObjectNotFound, bucket, key, r.VersionID)
			}
			bucketID, vk, rec = br.ID, k, r
			raw = bytes.Clone(bb.Bucket(subVersions).Get(k))
			return nil
		})
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if err := fn(&rec.Meta); err != nil {
			return storage.ObjectInfo{}, err
		}
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
	return b.deleteObject(ctx, bucket, key, versionID, nil)
}

// DeleteObjectIf is DeleteObject made conditional on check, which is called
// with the addressed version inside the delete's write transaction (see
// storage.ConditionalDeleter).
func (b *Backend) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if check == nil {
		return storage.DeleteResult{}, errors.New("fs: nil delete check")
	}
	return b.deleteObject(ctx, bucket, key, versionID, check)
}

// deleteObject implements DeleteObject and, with a non-nil check,
// DeleteObjectIf.
func (b *Backend) deleteObject(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
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
			if err != nil {
				return err
			}
			if err := check(cloneInfo(rec.info(bucket, key, latest))); err != nil {
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
