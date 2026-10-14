package s3client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

// DefaultPartSize is the default part size of Upload and Copy, and the
// largest object Upload sends with a single PUT.
const DefaultPartSize = 16 << 20

// UploadOptions configures Upload.
type UploadOptions struct {
	PutOptions
	// PartSize is the part size of multipart uploads and the largest size
	// sent with a single PUT (default DefaultPartSize, at least
	// MinPartSize). It grows automatically so no upload needs more than
	// MaxParts parts.
	PartSize int64
	// Concurrency is the number of parts uploaded in parallel (default 4).
	Concurrency int
}

// partSizeFor returns the part size to use for an object of size bytes.
func partSizeFor(size, want int64) int64 {
	if want <= 0 {
		want = DefaultPartSize
	}
	want = max(want, MinPartSize)
	if need := (size + MaxParts - 1) / MaxParts; need > want {
		// Round up to a whole MiB.
		want = (need + 1<<20 - 1) &^ (1<<20 - 1)
	}
	return want
}

// Upload stores r[0:size] as bucket/key: with one PUT up to the part size,
// as a parallel multipart upload above it (aborted again on failure). The
// preconditions apply to the final PUT or CompleteMultipartUpload, so they
// are evaluated atomically by the service. It returns the new ETag and
// whether a multipart upload was used (the ETag then is not the MD5 of the
// content).
func (c *Client) Upload(ctx context.Context, bucket, key string, r io.ReaderAt, size int64, opts UploadOptions) (etag string, multipart bool, err error) {
	partSize := partSizeFor(size, opts.PartSize)
	if size <= partSize {
		etag, err := c.PutObject(ctx, bucket, key, r, size, opts.PutOptions)
		return etag, false, err
	}
	header := http.Header{}
	for k, vs := range opts.Header {
		header[http.CanonicalHeaderKey(k)] = vs
	}
	uploadID, err := c.CreateMultipartUpload(ctx, bucket, key, header)
	if err != nil {
		return "", true, err
	}
	parts, err := c.runParts(ctx, size, partSize, opts.Concurrency, func(ctx context.Context, n int, off, length int64) (string, error) {
		return c.UploadPart(ctx, bucket, key, uploadID, n, io.NewSectionReader(r, off, length), length)
	})
	if err == nil {
		etag, err = c.CompleteMultipartUpload(ctx, bucket, key, uploadID, parts,
			CompleteOptions{IfNoneMatch: opts.IfNoneMatch, IfMatch: opts.IfMatch})
	}
	if err != nil {
		// Best effort, also when the caller's context is already done.
		_ = c.AbortMultipartUpload(context.WithoutCancel(ctx), bucket, key, uploadID)
		return "", true, err
	}
	return etag, true, nil
}

// runParts calls part for every part of an object of size bytes split into
// partSize parts, concurrency at a time, and returns the completed parts in
// order. The first error cancels the remaining parts.
func (c *Client) runParts(ctx context.Context, size, partSize int64, concurrency int,
	part func(ctx context.Context, n int, off, length int64) (string, error)) ([]CompletedPart, error) {
	if concurrency <= 0 {
		concurrency = 4
	}
	count := int((size + partSize - 1) / partSize)
	if count == 0 {
		count = 1
	}
	if count > MaxParts {
		return nil, errors.New("s3client: object needs more than 10000 parts")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	parts := make([]CompletedPart, count)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, concurrency)
	for i := 0; i < count; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			off := int64(i) * partSize
			length := min(partSize, size-off)
			etag, err := part(ctx, i+1, off, length)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				cancel()
				return
			}
			parts[i] = CompletedPart{Number: i + 1, ETag: etag}
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return parts, nil
}

// Copy copies srcKey (size bytes) to dstKey within bucket server-side:
// CopyObject up to MaxCopySize, a multipart copy above it. With
// opts.ReplaceMetadata the destination gets opts.Header; a multipart copy
// always uses opts.Header (it cannot keep the source's headers), so callers
// copying large objects must pass the complete header set. opts.IfMatch
// pins the source ETag.
func (c *Client) Copy(ctx context.Context, bucket, dstKey, srcKey string, size int64, opts CopyOptions) (string, error) {
	if size <= MaxCopySize {
		return c.CopyObject(ctx, bucket, dstKey, srcKey, opts)
	}
	header := http.Header{}
	for k, vs := range opts.Header {
		header[http.CanonicalHeaderKey(k)] = vs
	}
	uploadID, err := c.CreateMultipartUpload(ctx, bucket, dstKey, header)
	if err != nil {
		return "", err
	}
	partSize := partSizeFor(size, 512<<20)
	parts, err := c.runParts(ctx, size, partSize, 4, func(ctx context.Context, n int, off, length int64) (string, error) {
		return c.UploadPartCopy(ctx, bucket, dstKey, uploadID, n, srcKey, off, length, opts.IfMatch)
	})
	var etag string
	if err == nil {
		etag, err = c.CompleteMultipartUpload(ctx, bucket, dstKey, uploadID, parts, CompleteOptions{})
	}
	if err != nil {
		_ = c.AbortMultipartUpload(context.WithoutCancel(ctx), bucket, dstKey, uploadID)
		return "", err
	}
	return etag, nil
}
