package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// MaxPartNumber is the largest part number accepted (S3 allows 1..10000).
const MaxPartNumber = maxPartNumber

func uploadNotFound(uploadID string) error {
	return fmt.Errorf("%w: %.64q", storage.ErrUploadNotFound, uploadID)
}

// loadUpload returns the upload record of uploadID if it belongs to key.
func loadUpload(bb *bbolt.Bucket, key, uploadID string) (*uploadRecord, error) {
	if !validUploadID(uploadID) {
		return nil, uploadNotFound(uploadID)
	}
	v := bb.Bucket(subUploads).Get([]byte(uploadID))
	if v == nil {
		return nil, uploadNotFound(uploadID)
	}
	var rec uploadRecord
	if err := decodeRecord(v, &rec); err != nil {
		return nil, err
	}
	if rec.Key != key {
		return nil, uploadNotFound(uploadID)
	}
	return &rec, nil
}

// partFileName returns a fresh, unique file name for part n.
func partFileName(n int) string { return strconv.Itoa(n) + "-" + randomHex(8) }

// parsePartFileName returns the part number encoded in a part file name.
func parsePartFileName(name string) (int, bool) {
	num, rnd, ok := strings.Cut(name, "-")
	if !ok || !validDataID(rnd) {
		return 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || n > maxPartNumber || strconv.Itoa(n) != num {
		return 0, false
	}
	return n, true
}

// CreateMultipartUpload starts a multipart upload of bucket/key.
func (b *Backend) CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	if err := validKey(key); err != nil {
		return storage.UploadInfo{}, err
	}
	if err := b.begin(ctx); err != nil {
		return storage.UploadInfo{}, err
	}
	var info storage.UploadInfo
	err := b.update(func(tx *bbolt.Tx) error {
		br, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		global := tx.Bucket(topUploads)
		id := newUploadID()
		for global.Get([]byte(id)) != nil {
			id = newUploadID()
		}
		rec := uploadRecord{
			Key:       key,
			Initiated: time.Now().UTC(),
			Meta:      cloneMeta(meta),
			Initiator: initiator,
		}
		if err := putRecord(bb.Bucket(subUploads), []byte(id), &rec); err != nil {
			return err
		}
		if err := putUploadIndex(bb, id, &rec); err != nil {
			return err
		}
		if err := global.Put([]byte(id), []byte(br.ID)); err != nil {
			return err
		}
		info = rec.info(bucket, id)
		return nil
	})
	if err != nil {
		return storage.UploadInfo{}, err
	}
	info.Meta = cloneMeta(info.Meta)
	return info, nil
}

// GetMultipartUpload returns an in-progress upload of bucket/key.
func (b *Backend) GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error) {
	if err := b.begin(ctx); err != nil {
		return storage.UploadInfo{}, err
	}
	var info storage.UploadInfo
	err := b.view(func(tx *bbolt.Tx) error {
		_, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		rec, err := loadUpload(bb, key, uploadID)
		if err != nil {
			return err
		}
		info = rec.info(bucket, uploadID)
		return nil
	})
	return info, err
}

type partWriter struct {
	b        *Backend
	bucket   string
	bucketID string
	key      string
	uploadID string
	number   int

	mu   sync.Mutex
	t    *tmpFile
	done bool
}

// NewPartWriter starts writing part partNumber (1..MaxPartNumber) of an
// upload. The upload's existence is checked again at Commit.
func (b *Backend) NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	if partNumber < 1 || partNumber > maxPartNumber {
		return nil, fmt.Errorf("fs: invalid part number %d", partNumber)
	}
	if err := b.begin(ctx); err != nil {
		return nil, err
	}
	var id string
	err := b.view(func(tx *bbolt.Tx) error {
		br, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		if _, err := loadUpload(bb, key, uploadID); err != nil {
			return err
		}
		id = br.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &partWriter{b: b, bucket: bucket, bucketID: id, key: key, uploadID: uploadID, number: partNumber, t: b.newTemp()}, nil
}

func (w *partWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	return w.t.Write(p)
}

func (w *partWriter) Commit(ctx context.Context, meta storage.PartMeta) (storage.PartInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return storage.PartInfo{}, errWriterDone
	}
	w.done = true
	b := w.b
	if err := b.begin(ctx); err != nil {
		w.t.discard()
		return storage.PartInfo{}, err
	}
	// Parts always get a file (even when empty) so completion can treat
	// all parts alike.
	if err := w.t.finish(b.fsync, true); err != nil {
		return storage.PartInfo{}, err
	}
	file := partFileName(w.number)
	path := b.partPath(w.uploadID, file)
	b.pending.add(path)
	defer b.pending.done(path)
	if err := b.place(w.t.path, path); err != nil {
		w.t.remove()
		return storage.PartInfo{}, err
	}

	rec := partRecord{
		File:         file,
		Size:         w.t.size,
		LastModified: time.Now().UTC(),
		ETag:         meta.ETag,
		Checksums:    maps.Clone(meta.Checksums),
	}
	var oldFile string
	indeterminate, err := b.updateFiles(func(tx *bbolt.Tx) error {
		oldFile = ""
		_, bb, err := loadBucketID(tx, w.bucket, w.bucketID)
		if err != nil {
			return err
		}
		if _, err := loadUpload(bb, w.key, w.uploadID); err != nil {
			return err
		}
		parts := bb.Bucket(subParts)
		pk := partKey(w.uploadID, w.number)
		if v := parts.Get(pk); v != nil {
			var old partRecord
			if err := decodeRecord(v, &old); err == nil {
				if _, ok := parsePartFileName(old.File); ok {
					oldFile = old.File
				}
			}
		}
		return putRecord(parts, pk, &rec)
	})
	if err != nil {
		if indeterminate {
			// The part record may have been persisted; recovery removes
			// the file at the next start if it was not.
			b.log.Warn("fs: metadata commit failed after the transaction was written; keeping its part file",
				"upload_id", w.uploadID, "part", w.number, "err", err)
			return storage.PartInfo{}, err
		}
		_ = os.Remove(path)
		if errors.Is(err, storage.ErrUploadNotFound) || errors.Is(err, storage.ErrBucketNotFound) {
			// The upload is gone; drop the directory we may have recreated.
			_ = os.Remove(b.uploadPath(w.uploadID))
		}
		return storage.PartInfo{}, err
	}
	if oldFile != "" && oldFile != file {
		if err := os.Remove(b.partPath(w.uploadID, oldFile)); err != nil && !errors.Is(err, iofs.ErrNotExist) {
			b.log.Warn("fs: remove replaced part", "upload_id", w.uploadID, "part", w.number, "err", err)
		}
	}
	info := rec.info(w.number)
	info.Checksums = maps.Clone(info.Checksums)
	return info, nil
}

func (w *partWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return nil
	}
	w.done = true
	w.t.discard()
	return nil
}

// ListParts returns all committed parts sorted by part number.
func (b *Backend) ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error) {
	if err := b.begin(ctx); err != nil {
		return nil, err
	}
	var out []storage.PartInfo
	err := b.view(func(tx *bbolt.Tx) error {
		out = []storage.PartInfo{}
		_, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		if _, err := loadUpload(bb, key, uploadID); err != nil {
			return err
		}
		return forEachPart(bb, uploadID, func(n int, rec *partRecord) error {
			out = append(out, rec.info(n))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// forEachPart calls fn for every part record of an upload in part order.
func forEachPart(bb *bbolt.Bucket, uploadID string, fn func(n int, rec *partRecord) error) error {
	p := []byte(uploadID)
	c := bb.Bucket(subParts).Cursor()
	for k, v := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, v = c.Next() {
		if len(k) != len(p)+4 {
			continue
		}
		n := int(uint32(k[len(p)])<<24 | uint32(k[len(p)+1])<<16 | uint32(k[len(p)+2])<<8 | uint32(k[len(p)+3]))
		var rec partRecord
		if err := decodeRecord(v, &rec); err != nil {
			return err
		}
		if err := fn(n, &rec); err != nil {
			return err
		}
	}
	return nil
}

// ListMultipartUploads returns all in-progress uploads sorted by key, then
// initiation time (oldest first), then upload ID.
// The result is a snapshot read in one transaction from the listing-order
// index; use WalkMultipartUploads (or storage.ListUploads) to read a page
// without loading every upload.
func (b *Backend) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	if err := b.begin(ctx); err != nil {
		return nil, err
	}
	var out []storage.UploadInfo
	err := b.view(func(tx *bbolt.Tx) error {
		out = []storage.UploadInfo{}
		_, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		upl := bb.Bucket(subUploads)
		return bb.Bucket(subUploadOrder).ForEach(func(_, id []byte) error {
			raw := upl.Get(id)
			if raw == nil {
				return nil
			}
			var rec uploadRecord
			if err := decodeRecord(raw, &rec); err != nil {
				return err
			}
			out = append(out, rec.info(bucket, string(id)))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// deleteUploadTx removes an upload (record rec) and all its part records.
func deleteUploadTx(tx *bbolt.Tx, bb *bbolt.Bucket, uploadID string, rec *uploadRecord) error {
	if err := bb.Bucket(subUploads).Delete([]byte(uploadID)); err != nil {
		return err
	}
	if err := deleteUploadIndex(bb, uploadID, rec); err != nil {
		return err
	}
	if err := tx.Bucket(topUploads).Delete([]byte(uploadID)); err != nil {
		return err
	}
	parts := bb.Bucket(subParts)
	p := []byte(uploadID)
	var keys [][]byte
	c := parts.Cursor()
	for k, _ := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, _ = c.Next() {
		keys = append(keys, bytes.Clone(k))
	}
	for _, k := range keys {
		if err := parts.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// removeUploadDir removes an upload's part directory after its records are
// gone.
func (b *Backend) removeUploadDir(uploadID string) {
	if !validUploadID(uploadID) {
		return
	}
	if err := os.RemoveAll(b.uploadPath(uploadID)); err != nil {
		b.log.Warn("fs: remove upload directory", "upload_id", uploadID, "err", err)
	}
}

// completeRetries bounds how often CompleteMultipartUpload starts over
// when a listed part is re-uploaded while its data is being copied.
const completeRetries = 4

// afterPartsCopied, when non-nil, runs after CompleteMultipartUpload copied
// the parts and before it commits (tests use it to inject races).
var afterPartsCopied func(uploadID string)

// errPartsChanged reports that a part was replaced during completion.
var errPartsChanged = errors.New("fs: multipart upload parts changed during completion")

// CompleteMultipartUpload concatenates the listed parts into a new object
// version (committed like ObjectWriter.Commit) and removes the upload with
// all its parts. If the commit fails (e.g. preconditions) the upload is
// kept, so the client may retry.
//
// The commit transaction verifies that every listed part still refers to the
// file that was copied; if a part was re-uploaded concurrently the
// completion starts over (a bounded number of times), so the committed
// object always consists of the parts current at commit time.
func (b *Backend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for attempt := 1; ; attempt++ {
		info, err := b.completeOnce(ctx, bucket, key, uploadID, partNumbers, meta, opts)
		if errors.Is(err, errPartsChanged) && attempt < completeRetries {
			continue
		}
		if errors.Is(err, errPartsChanged) {
			return storage.ObjectInfo{}, fmt.Errorf("fs: complete upload %q: %w (too much contention)", uploadID, err)
		}
		return info, err
	}
}

// completeOnce is one attempt of CompleteMultipartUpload. It returns
// errPartsChanged when a listed part was replaced or vanished while the
// upload still exists.
func (b *Backend) completeOnce(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	if err := b.begin(ctx); err != nil {
		return storage.ObjectInfo{}, err
	}
	if opts.PartETags != nil && len(opts.PartETags) != len(partNumbers) {
		return storage.ObjectInfo{}, fmt.Errorf("fs: %d part ETags for %d parts", len(opts.PartETags), len(partNumbers))
	}
	// Resolve the parts and fail fast on preconditions before copying data.
	// The part ETags are checked here; the commit below re-checks that every
	// part is still the same file, so a part replaced in between makes the
	// next attempt see (and reject) its new ETag.
	var (
		bucketID string
		files    []string
		sizes    []int64
	)
	err := b.view(func(tx *bbolt.Tx) error {
		files, sizes = files[:0], sizes[:0]
		br, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		if _, err := loadUpload(bb, key, uploadID); err != nil {
			return err
		}
		bucketID = br.ID
		parts := bb.Bucket(subParts)
		for i, n := range partNumbers {
			var v []byte
			if n >= 1 && n <= maxPartNumber {
				v = parts.Get(partKey(uploadID, n))
			}
			if v == nil {
				return fmt.Errorf("%w: part %d of upload %q", storage.ErrPartNotFound, n, uploadID)
			}
			var rec partRecord
			if err := decodeRecord(v, &rec); err != nil {
				return err
			}
			if _, ok := parsePartFileName(rec.File); !ok {
				return fmt.Errorf("%w: invalid part file name", errCorrupt)
			}
			if opts.PartETags != nil && rec.ETag != opts.PartETags[i] {
				return fmt.Errorf("%w: part %d of upload %q was replaced (ETag %q, want %q)",
					storage.ErrPartNotFound, n, uploadID, rec.ETag, opts.PartETags[i])
			}
			files = append(files, rec.File)
			sizes = append(sizes, rec.Size)
		}
		return checkPreconditions(bb, key, opts)
	})
	if err != nil {
		return storage.ObjectInfo{}, err
	}

	t := b.newTemp()
	for i, file := range files {
		if err := b.appendPart(ctx, t, uploadID, file, sizes[i], partNumbers[i]); err != nil {
			t.discard()
			if errors.Is(err, errPartsChanged) {
				// The part file vanished: the part was replaced (retry) or
				// the upload was aborted or completed concurrently (report
				// that instead).
				if _, gerr := b.GetMultipartUpload(ctx, bucket, key, uploadID); gerr != nil {
					return storage.ObjectInfo{}, gerr
				}
			}
			return storage.ObjectInfo{}, err
		}
	}
	dataID, path, err := b.finishData(t, bucketID)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if path != "" {
		defer b.pending.done(path)
	}
	if afterPartsCopied != nil {
		afterPartsCopied(uploadID)
	}
	info, err := b.commitObject(commitReq{
		bucket:   bucket,
		bucketID: bucketID,
		key:      key,
		dataID:   dataID,
		size:     t.size,
		meta:     cloneMeta(meta),
		opts:     opts,
		pre: func(tx *bbolt.Tx, bb *bbolt.Bucket) error {
			up, err := loadUpload(bb, key, uploadID)
			if err != nil {
				return err
			}
			parts := bb.Bucket(subParts)
			for i, n := range partNumbers {
				v := parts.Get(partKey(uploadID, n))
				if v == nil {
					return fmt.Errorf("%w: part %d of upload %q", storage.ErrPartNotFound, n, uploadID)
				}
				var rec partRecord
				if err := decodeRecord(v, &rec); err != nil {
					return err
				}
				if rec.File != files[i] {
					return errPartsChanged
				}
			}
			return deleteUploadTx(tx, bb, uploadID, up)
		},
	})
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	b.removeUploadDir(uploadID)
	return info, nil
}

// appendPart copies one part file into t, verifying its size. The copy goes
// file to file (copy_file_range on Linux), so the context is checked between
// parts.
func (b *Backend) appendPart(ctx context.Context, t *tmpFile, uploadID, file string, size int64, n int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := openShared(b.partPath(uploadID, file))
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) || openRaced(err) {
			// Replaced, aborted or completed concurrently.
			return fmt.Errorf("%w: part %d vanished", errPartsChanged, n)
		}
		return fmt.Errorf("fs: open part %d: %w", n, err)
	}
	defer f.Close()
	// Bypass t's buffer: flush it, then copy straight into the file.
	if err := t.open(); err != nil {
		return err
	}
	if err := t.flush(); err != nil {
		return err
	}
	copied, err := io.Copy(t.f, io.LimitReader(f, size+1))
	t.size += copied
	if err != nil {
		t.err = err
		return fmt.Errorf("fs: copy part %d: %w", n, err)
	}
	if copied != size {
		return fmt.Errorf("%w: part %d of upload %q has %d bytes, expected %d", errCorrupt, n, uploadID, copied, size)
	}
	return nil
}

// AbortMultipartUpload removes the upload and its parts.
func (b *Backend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	if err := b.begin(ctx); err != nil {
		return err
	}
	err := b.update(func(tx *bbolt.Tx) error {
		_, bb, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		up, err := loadUpload(bb, key, uploadID)
		if err != nil {
			return err
		}
		return deleteUploadTx(tx, bb, uploadID, up)
	})
	if err != nil {
		return err
	}
	b.removeUploadDir(uploadID)
	return nil
}
