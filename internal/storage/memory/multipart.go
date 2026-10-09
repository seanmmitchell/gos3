package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
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
	bk.uploads[id] = u
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
	for _, u := range bk.uploads {
		out = append(out, cloneUpload(u.info))
	}
	slices.SortFunc(out, func(a, c storage.UploadInfo) int {
		if r := strings.Compare(a.Key, c.Key); r != 0 {
			return r
		}
		if r := a.Initiated.Compare(c.Initiated); r != 0 {
			return r
		}
		return strings.Compare(a.UploadID, c.UploadID)
	})
	return out, nil
}

// CompleteMultipartUpload concatenates the listed parts into a new object
// version (committed like ObjectWriter.Commit) and removes the upload with
// all its parts. If the commit preconditions fail the upload is kept, so the
// client may retry.
func (b *Backend) CompleteMultipartUpload(ctx context.Context, bucketName, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
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
	for _, n := range partNumbers {
		p, ok := u.parts[n]
		if !ok {
			return storage.ObjectInfo{}, fmt.Errorf("%w: part %d of upload %q", storage.ErrPartNotFound, n, uploadID)
		}
		parts = append(parts, p)
		total += int64(len(p.data))
	}
	obj, _ := bk.objects.Get(key)
	if err := checkPreconditions(obj, opts); err != nil {
		return storage.ObjectInfo{}, err
	}
	data := make([]byte, 0, total)
	for _, p := range parts {
		data = append(data, p.data...)
	}
	info, err := b.commitLocked(bk, key, data, meta, opts)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	delete(bk.uploads, uploadID)
	return info, nil
}

// AbortMultipartUpload removes the upload and its parts.
func (b *Backend) AbortMultipartUpload(ctx context.Context, bucketName, key, uploadID string) error {
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return err
	}
	defer bk.mu.Unlock()
	if _, err := findUpload(bk, key, uploadID); err != nil {
		return err
	}
	delete(bk.uploads, uploadID)
	return nil
}
