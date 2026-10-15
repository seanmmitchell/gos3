package s3client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Reader is a lazy, seekable reader over one remote object version: it
// issues a ranged GET from the current offset on the first Read after
// creation or a Seek, pinned to the version ID or else the ETag it was
// created with (a changed object fails with a precondition error instead of
// returning mixed content). A broken body is reopened once at the current
// offset.
type Reader struct {
	c           *Client
	ctx         context.Context
	bucket, key string
	size        int64
	etag        string
	versionID   string // read this version ("" = the current one, pinned by etag)

	mu     sync.Mutex
	off    int64
	body   io.ReadCloser
	closed bool
}

// NewReader returns a reader over bucket/key, which has size bytes and the
// given ETag ("" = do not pin). ctx bounds every request of the reader.
func (c *Client) NewReader(ctx context.Context, bucket, key string, size int64, etag string) *Reader {
	return &Reader{c: c, ctx: ctx, bucket: bucket, key: key, size: size, etag: etag}
}

// NewVersionReader is NewReader pinned to a version: every request reads
// versionID when it is not "" or "null" (a version's content never
// changes, so the ETag is not checked then); otherwise the reader reads
// the current object pinned to etag, as NewReader. ("null" names the
// version written while versioning was off or suspended, which a later
// write replaces in place.)
func (c *Client) NewVersionReader(ctx context.Context, bucket, key string, size int64, versionID, etag string) *Reader {
	r := c.NewReader(ctx, bucket, key, size, etag)
	if versionID != "" && versionID != "null" {
		r.versionID, r.etag = versionID, ""
	}
	return r
}

var errReaderClosed = errors.New("s3client: read from a closed reader")

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errReaderClosed
	}
	if r.off >= r.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > r.size-r.off {
		p = p[:r.size-r.off]
	}
	for attempt := 0; ; attempt++ {
		if r.body == nil {
			body, _, err := r.c.GetObject(r.ctx, r.bucket, r.key, GetOptions{Offset: r.off, IfMatch: r.etag, VersionID: r.versionID})
			if err != nil {
				return 0, err
			}
			r.body = body
		}
		n, err := r.body.Read(p)
		r.off += int64(n)
		switch {
		case err == nil:
			return n, nil
		case n > 0:
			// Return the data; a premature end or a broken body is
			// reopened at the current offset by the next Read.
			_ = r.closeBody()
			return n, nil
		}
		_ = r.closeBody()
		if errors.Is(err, io.EOF) {
			// The body ended before the expected size.
			return 0, r.checkShort(io.EOF)
		}
		if attempt > 0 || r.ctx.Err() != nil {
			return 0, err
		}
	}
}

// checkShort turns a premature EOF into io.ErrUnexpectedEOF.
func (r *Reader) checkShort(err error) error {
	if errors.Is(err, io.EOF) && r.off < r.size {
		return fmt.Errorf("s3client: %s/%s ended at %d of %d bytes: %w", r.bucket, r.key, r.off, r.size, io.ErrUnexpectedEOF)
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (r *Reader) closeBody() error {
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}

// Seek implements io.Seeker. Seeking is free; the next Read reopens the
// object at the new offset if it changed.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errReaderClosed
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, errors.New("s3client: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("s3client: negative position")
	}
	if abs != r.off {
		_ = r.closeBody()
		r.off = abs
	}
	return abs, nil
}

// Close releases the open response body, if any. It is idempotent.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	_ = r.closeBody()
	return nil
}
