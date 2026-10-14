package memory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
)

// MaxPartNumber is the largest part number accepted (S3 allows 1..10000).
const MaxPartNumber = 10000

// upload is an in-progress multipart upload. Guarded by its bucket's lock.
type upload struct {
	info  storage.UploadInfo
	parts map[int]*part
}

// part is one committed part; data is immutable.
type part struct {
	info storage.PartInfo
	data []byte
}

// findUpload returns the upload with the given ID if it belongs to key.
func findUpload(bk *bucket, key, uploadID string) (*upload, error) {
	u, ok := bk.uploads[uploadID]
	if !ok || u.info.Key != key {
		return nil, fmt.Errorf("%w: %q", storage.ErrUploadNotFound, uploadID)
	}
	return u, nil
}

// CreateMultipartUpload starts a multipart upload of bucket/key.
func (b *Backend) CreateMultipartUpload(ctx context.Context, bucketName, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error) {
	if err := validKey(key); err != nil {
		return storage.UploadInfo{}, err
	}
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	defer bk.mu.Unlock()
	id := newUploadID()
	for _, dup := bk.uploads[id]; dup; _, dup = bk.uploads[id] {
		id = newUploadID()
	}
	u := &upload{
		info: storage.UploadInfo{
			UploadID:  id,
			Bucket:    bk.info.Name,
			Key:       key,
			Initiated: b.now().UTC(),
			Meta:      cloneMeta(meta),
			Initiator: initiator,
		},
		parts: make(map[int]*part),
	}
	bk.addUpload(u)
	return cloneUpload(u.info), nil
}

// GetMultipartUpload returns an in-progress upload of bucket/key.
func (b *Backend) GetMultipartUpload(ctx context.Context, bucketName, key, uploadID string) (storage.UploadInfo, error) {
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	defer bk.mu.RUnlock()
	u, err := findUpload(bk, key, uploadID)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	return cloneUpload(u.info), nil
}

type partWriter struct {
	b        *Backend
	bk       *bucket
	key      string
	uploadID string
	number   int
	mu       sync.Mutex
	buf      []byte
	done     bool
}

// NewPartWriter starts writing part partNumber (1..MaxPartNumber) of an
// upload. The upload's existence is checked again at Commit.
func (b *Backend) NewPartWriter(ctx context.Context, bucketName, key, uploadID string, partNumber int) (storage.PartWriter, error) {
	if partNumber < 1 || partNumber > MaxPartNumber {
		return nil, fmt.Errorf("memory: invalid part number %d", partNumber)
	}
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return nil, err
	}
	_, err = findUpload(bk, key, uploadID)
	bk.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return &partWriter{b: b, bk: bk, key: key, uploadID: uploadID, number: partNumber}, nil
}

func (w *partWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, errWriterDone
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *partWriter) Commit(ctx context.Context, meta storage.PartMeta) (storage.PartInfo, error) {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return storage.PartInfo{}, errWriterDone
	}
	w.done = true
	data := slices.Clip(w.buf)
	w.buf = nil
	w.mu.Unlock()
	if data == nil {
		data = []byte{}
	}

	if err := w.b.relock(ctx, w.bk); err != nil {
		return storage.PartInfo{}, err
	}
	defer w.bk.mu.Unlock()
	u, err := findUpload(w.bk, w.key, w.uploadID)
	if err != nil {
		return storage.PartInfo{}, err
	}
	p := &part{
		info: storage.PartInfo{
			PartMeta:     clonePartMeta(meta),
			PartNumber:   w.number,
			Size:         int64(len(data)),
			LastModified: w.b.now().UTC(),
		},
		data: data,
	}
	u.parts[w.number] = p
	out := p.info
	out.PartMeta = clonePartMeta(out.PartMeta)
	return out, nil
}

func (w *partWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	w.buf = nil
	return nil
}

// ListParts returns all committed parts sorted by part number.
func (b *Backend) ListParts(ctx context.Context, bucketName, key, uploadID string) ([]storage.PartInfo, error) {
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return nil, err
	}
	defer bk.mu.RUnlock()
	u, err := findUpload(bk, key, uploadID)
	if err != nil {
		return nil, err
	}
	out := make([]storage.PartInfo, 0, len(u.parts))
	for _, p := range u.parts {
		pi := p.info
		pi.PartMeta = clonePartMeta(pi.PartMeta)
		out = append(out, pi)
	}
	slices.SortFunc(out, func(a, c storage.PartInfo) int { return a.PartNumber - c.PartNumber })
	return out, nil
}

// ListMultipartUploads returns all in-progress uploads sorted by key, then
// initiation time (oldest first), then upload ID.
func (b *Backend) ListMultipartUploads(ctx context.Context, bucketName string) ([]storage.UploadInfo, error) {
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return nil, err
	}
	defer bk.mu.RUnlock()
	out := make([]storage.UploadInfo, 0, len(bk.uploads))
	for n := bk.uploadOrder.First(); n != nil; n = n.Next() {
		out = append(out, cloneUpload(n.val.info))
	}
	return out, nil
}

// WalkMultipartUploads calls fn for the uploads with the given key prefix
// after position after, in listing order (see storage.UploadWalker). The
// uploads are read in batches under the bucket's read lock; fn runs with
// no lock held.
func (b *Backend) WalkMultipartUploads(ctx context.Context, bucketName, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	if fn == nil {
		return errors.New("memory: nil walk function")
	}
	start := string(appendEscapedKey(nil, prefix))
	cursor, inclusive := uploadSeekKey(after)
	if cursor < start {
		cursor, inclusive = start, true
	}
	batch := walkMinBatch
	for {
		bk, err := b.rlockBucket(ctx, bucketName)
		if err != nil {
			return err
		}
		var n *slNode[*upload]
		if inclusive {
			n = bk.uploadOrder.SeekGE(cursor)
		} else {
			n = bk.uploadOrder.SeekGT(cursor)
		}
		items := make([]storage.UploadInfo, 0, batch)
		done := true
		for ; n != nil && strings.HasPrefix(n.key, start); n = n.Next() {
			if len(items) == batch {
				done = false
				break
			}
			cursor, inclusive = n.key, false
			if strings.HasPrefix(n.val.info.Key, prefix) {
				items = append(items, cloneUpload(n.val.info))
			}
		}
		bk.mu.RUnlock()
		for _, it := range items {
			if err := fn(it); err != nil {
				if errors.Is(err, storage.ErrStopWalk) {
					return nil
				}
				return err
			}
		}
		if done {
			return nil
		}
		batch = nextBatch(batch)
	}
}

// CompleteMultipartUpload concatenates the listed parts into a new object
// version (committed like ObjectWriter.Commit) and removes the upload with
// all its parts. If the commit preconditions fail the upload is kept, so the
// client may retry.
func (b *Backend) CompleteMultipartUpload(ctx context.Context, bucketName, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	return b.CompleteMultipartUploadWith(ctx, bucketName, key, uploadID, partNumbers, meta, opts, storage.WriteOptions{})
}

// CompleteMultipartUploadWith is CompleteMultipartUpload with write
// options: with wopts.Compression the concatenated parts are stored
// compressed (see storage.CompressingBackend). The compression runs with
// the bucket locked, like the concatenation.
func (b *Backend) CompleteMultipartUploadWith(ctx context.Context, bucketName, key, uploadID string, partNumbers []int,
	meta storage.ObjectMeta, opts storage.CommitOptions, wopts storage.WriteOptions) (storage.ObjectInfo, error) {
	if err := checkCompression(wopts.Compression); err != nil {
		return storage.ObjectInfo{}, err
	}
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	defer bk.mu.Unlock()
	u, err := findUpload(bk, key, uploadID)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	var total int64
	parts := make([]*part, 0, len(partNumbers))
	if opts.PartETags != nil && len(opts.PartETags) != len(partNumbers) {
		return storage.ObjectInfo{}, fmt.Errorf("memory: %d part ETags for %d parts", len(opts.PartETags), len(partNumbers))
	}
	for i, n := range partNumbers {
		p, ok := u.parts[n]
		if !ok {
			return storage.ObjectInfo{}, fmt.Errorf("%w: part %d of upload %q", storage.ErrPartNotFound, n, uploadID)
		}
		if opts.PartETags != nil && p.info.ETag != opts.PartETags[i] {
			return storage.ObjectInfo{}, fmt.Errorf("%w: part %d of upload %q was replaced (ETag %q, want %q)",
				storage.ErrPartNotFound, n, uploadID, p.info.ETag, opts.PartETags[i])
		}
		parts = append(parts, p)
		total += int64(len(p.data))
	}
	obj, _ := bk.objects.Get(key)
	if err := checkPreconditions(obj, opts); err != nil {
		return storage.ObjectInfo{}, err
	}
	var (
		data []byte
		enc  stored
	)
	if c := wopts.Compression; c.Enabled() && total > 0 {
		zw, err := compress.NewWriter((*appendWriter)(&data), c.Level)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		for _, p := range parts {
			_, _ = zw.Write(p.data) // appending to a slice cannot fail
		}
		if err := zw.Close(); err != nil {
			return storage.ObjectInfo{}, err
		}
		enc = stored{compression: c.Algorithm, size: total}
	} else {
		data = make([]byte, 0, total)
		for _, p := range parts {
			data = append(data, p.data...)
		}
	}
	meta.Encryption = wopts.Encryption
	info, err := b.commitLocked(bk, key, data, enc, meta, opts)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	bk.removeUpload(u)
	return info, nil
}

// AbortMultipartUpload removes the upload and its parts.
func (b *Backend) AbortMultipartUpload(ctx context.Context, bucketName, key, uploadID string) error {
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return err
	}
	defer bk.mu.Unlock()
	u, err := findUpload(bk, key, uploadID)
	if err != nil {
		return err
	}
	bk.removeUpload(u)
	return nil
}

// addUpload registers u (bucket lock held).
func (bk *bucket) addUpload(u *upload) {
	bk.uploads[u.info.UploadID] = u
	bk.uploadOrder.Set(uploadOrderKey(u.info.Key, u.info.Initiated, u.info.UploadID), u)
}

// removeUpload unregisters u (bucket lock held).
func (bk *bucket) removeUpload(u *upload) {
	delete(bk.uploads, u.info.UploadID)
	bk.uploadOrder.Delete(uploadOrderKey(u.info.Key, u.info.Initiated, u.info.UploadID))
}

// appendEscapedKey appends key with every 0x00 byte escaped as 0x00 0xFF.
// Followed by a terminator starting with 0x00 and a byte below 0xFF, the
// byte-wise order of escaped keys is the order of the keys and no escaped
// key is a prefix of another key's terminated encoding.
func appendEscapedKey(dst []byte, key string) []byte {
	for i := 0; i < len(key); i++ {
		dst = append(dst, key[i])
		if key[i] == 0 {
			dst = append(dst, 0xff)
		}
	}
	return dst
}

// uploadOrderKey returns the uploadOrder key of an upload: the escaped
// object key, the terminator 0x00 0x01, the initiation time (sign-flipped
// big-endian Unix seconds and nanoseconds) and the upload ID, so that the
// byte-wise order of keys is the listing order (key, Initiated, UploadID).
func uploadOrderKey(key string, initiated time.Time, uploadID string) string {
	b := make([]byte, 0, len(key)+14+len(uploadID))
	b = appendEscapedKey(b, key)
	b = append(b, 0x00, 0x01)
	b = binary.BigEndian.AppendUint64(b, uint64(initiated.Unix())^(1<<63))
	b = binary.BigEndian.AppendUint32(b, uint32(initiated.Nanosecond()))
	return string(append(b, uploadID...))
}

// uploadSeekKey returns where a walk after position p starts: at the
// returned key (inclusive) or strictly after it.
func uploadSeekKey(p storage.UploadPosition) (key string, inclusive bool) {
	switch {
	case p == storage.UploadPosition{}:
		return "", true
	case p.AfterKey:
		// 0x00 0x02 sorts after the terminator 0x00 0x01 of every upload of
		// p.Key and before every escaped greater key.
		return string(append(appendEscapedKey(nil, p.Key), 0x00, 0x02)), true
	default:
		return uploadOrderKey(p.Key, p.Initiated, p.UploadID), false
	}
}
