package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
	"github.com/seanmmitchell/gos3/internal/storage/index"
)

// MaxPartNumber is the largest part number accepted (S3 allows 1..10000).
const MaxPartNumber = 10000

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
	u := bk.ix.AddUpload(storage.UploadInfo{
		UploadID:  bk.ix.NewUploadID(),
		Bucket:    bk.ix.Info.Name,
		Key:       key,
		Initiated: b.now().UTC(),
		Meta:      meta,
		Initiator: initiator,
	})
	return index.CloneUpload(u.Info), nil
}

// GetMultipartUpload returns an in-progress upload of bucket/key.
func (b *Backend) GetMultipartUpload(ctx context.Context, bucketName, key, uploadID string) (storage.UploadInfo, error) {
	bk, err := b.rlockBucket(ctx, bucketName)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	defer bk.mu.RUnlock()
	u, err := bk.ix.Upload(key, uploadID)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	return index.CloneUpload(u.Info), nil
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
	_, err = bk.ix.Upload(key, uploadID)
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
	u, err := w.bk.ix.Upload(w.key, w.uploadID)
	if err != nil {
		return storage.PartInfo{}, err
	}
	p := &index.Part[[]byte]{
		Info: storage.PartInfo{
			PartMeta:     index.ClonePartMeta(meta),
			PartNumber:   w.number,
			Size:         int64(len(data)),
			LastModified: w.b.now().UTC(),
		},
		Data: data,
	}
	u.PutPart(p)
	out := p.Info
	out.PartMeta = index.ClonePartMeta(out.PartMeta)
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
	u, err := bk.ix.Upload(key, uploadID)
	if err != nil {
		return nil, err
	}
	parts := u.Parts()
	out := make([]storage.PartInfo, 0, len(parts))
	for _, p := range parts {
		pi := p.Info
		pi.PartMeta = index.ClonePartMeta(pi.PartMeta)
		out = append(out, pi)
	}
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
	uploads := bk.ix.Uploads()
	out := make([]storage.UploadInfo, 0, len(uploads))
	for _, u := range uploads {
		out = append(out, index.CloneUpload(u.Info))
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
	return index.WalkUploads(ctx, b.walkAcquire(ctx, bucketName), prefix, after, fn)
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
	u, err := bk.ix.Upload(key, uploadID)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	parts, err := u.SelectParts(partNumbers, opts.PartETags)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	var total int64
	for _, p := range parts {
		total += int64(len(p.Data))
	}
	if err := bk.ix.CheckPreconditions(key, opts); err != nil {
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
			_, _ = zw.Write(p.Data) // appending to a slice cannot fail
		}
		if err := zw.Close(); err != nil {
			return storage.ObjectInfo{}, err
		}
		enc = stored{compression: c.Algorithm, size: total}
	} else {
		data = make([]byte, 0, total)
		for _, p := range parts {
			data = append(data, p.Data...)
		}
	}
	meta.Encryption = wopts.Encryption
	info, err := b.commitLocked(bk, key, data, enc, meta, opts)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	bk.ix.RemoveUpload(uploadID)
	return info, nil
}

// AbortMultipartUpload removes the upload and its parts.
func (b *Backend) AbortMultipartUpload(ctx context.Context, bucketName, key, uploadID string) error {
	bk, err := b.lockBucket(ctx, bucketName)
	if err != nil {
		return err
	}
	defer bk.mu.Unlock()
	if _, err := bk.ix.Upload(key, uploadID); err != nil {
		return err
	}
	bk.ix.RemoveUpload(uploadID)
	return nil
}
