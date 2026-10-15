package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Blobs.
//
// Object data, multipart parts and the gateway's staged parts are immutable
// remote objects ("blobs"):
//
//	data/<bucket-id>/<data-id>           object version data (as stored: compressed or not)
//	parts/<upload-id>/<nnnnn>-<id>       multipart part data
//	gateway/parts/<ns>/<name>            gateway staged parts (AuxStore)
//
// A blob is uploaded before the journal record that references it, and
// registered as pending meanwhile, so the orphan recovery pass leaves it
// alone. A blob dereferenced by a durable record is deleted, unless open
// readers hold it (blobTracker): the last reader deletes it on Close.

// blobRef is the index payload of versions and parts: the stored blob.
type blobRef struct {
	// ID is the data ID of the blob ("" = empty content, no blob).
	ID string `json:"id,omitempty"`
	// Size is the number of stored bytes.
	Size int64 `json:"n,omitempty"`
	// Restic is the snapshot ID of a version archived in the bucket's
	// restic repository (storage.EncryptionRestic; ID is then "").
	Restic string `json:"restic,omitempty"`
	// Enc is storage.EncryptionAES256 for a part blob stored encrypted
	// (versions record their encoding in ObjectMeta.Encryption).
	Enc string `json:"enc,omitempty"`
}

const (
	blobDataDir   = "data/"
	blobPartsDir  = "parts/"
	blobStagedDir = "gateway/parts/"
)

func (b *Backend) dataKey(bucketID, dataID string) string {
	return b.keys.prefix + blobDataDir + bucketID + "/" + dataID
}

func (b *Backend) partPrefix(uploadID string) string {
	return b.keys.prefix + blobPartsDir + uploadID + "/"
}

func (b *Backend) partKey(uploadID string, n int, id string) string {
	return fmt.Sprintf("%s%05d-%s", b.partPrefix(uploadID), n, id)
}

func (b *Backend) stagedKey(ns, name string) string {
	return b.keys.prefix + blobStagedDir + ns + "/" + name
}

// parsePartName splits "<nnnnn>-<id>".
func parsePartName(s string) (int, string, bool) {
	ns, id, ok := strings.Cut(s, "-")
	if !ok || len(ns) != 5 || !validHexID(id) {
		return 0, "", false
	}
	n, err := strconv.Atoi(ns)
	return n, id, err == nil && n >= 1 && n <= MaxPartNumber
}

// validHexID reports whether s looks like an ID this package generates.
func validHexID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// --- errors ---

// errBlobMissing reports object data whose blob is gone.
var errBlobMissing = errors.New("s3store: object data is missing from the remote")

// mapErr turns a remote failure into the backend's error vocabulary:
// unreachable or failing services wrap storage.ErrBackingUnavailable; the
// context's own error is returned as such.
func (b *Backend) mapErr(ctx context.Context, what string, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if s3client.IsUnavailable(err) {
		return fmt.Errorf("%w: s3 backing %s: %s: %w", storage.ErrBackingUnavailable, b.Location(), what, err)
	}
	return fmt.Errorf("s3store: %s: %s: %w", b.Location(), what, err)
}

// mayHaveLanded reports whether a failed write request may nevertheless
// have been performed: anything but an error answer of the service.
func mayHaveLanded(err error) bool {
	var e *s3client.Error
	return !errors.As(err, &e) || e.StatusCode == 0 || e.StatusCode >= 500
}

// --- pending uploads ---

// pendingSet holds the keys of blobs being written whose referencing
// record is not durable yet.
type pendingSet struct {
	mu sync.Mutex
	m  map[string]int
}

func (p *pendingSet) add(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = make(map[string]int)
	}
	p.m[key]++
}

func (p *pendingSet) done(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m[key] <= 1 {
		delete(p.m, key)
		return
	}
	p.m[key]--
}

func (p *pendingSet) has(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.m[key] > 0
}

// --- open readers ---

// blobTracker counts open readers per blob and defers deleting a blob that
// is dereferenced while readers are open until the last one is closed.
type blobTracker struct {
	mu   sync.Mutex
	refs map[string]*trackedBlob
}

type trackedBlob struct {
	readers       int
	deleteOnClose bool
}

func (t *blobTracker) acquire(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.refs == nil {
		t.refs = make(map[string]*trackedBlob)
	}
	ref := t.refs[key]
	if ref == nil {
		ref = &trackedBlob{}
		t.refs[key] = ref
	}
	ref.readers++
}

// release unregisters a reader and reports whether the blob must be
// deleted now (it was dereferenced and this was its last reader).
func (t *blobTracker) release(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ref := t.refs[key]
	if ref == nil {
		return false
	}
	ref.readers--
	if ref.readers > 0 {
		return false
	}
	delete(t.refs, key)
	return ref.deleteOnClose
}

// deleteOrDefer reports whether a dereferenced blob may be deleted now; if
// readers hold it, the last of them deletes it instead.
func (t *blobTracker) deleteOrDefer(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ref := t.refs[key]; ref != nil && ref.readers > 0 {
		ref.deleteOnClose = true
		return false
	}
	return true
}

func (t *blobTracker) held(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ref := t.refs[key]
	return ref != nil && ref.readers > 0
}

// releaseBlob unregisters a reader of key and performs a deferred delete.
func (b *Backend) releaseBlob(key string) {
	if b.blobs.release(key) {
		b.deleteLater(key)
	}
}

// dropOnDurable registers keys as being removed by a record that is not
// durable yet (the recovery pass must treat them as referenced: the
// record may still fail) and returns the record's durable callback, which
// deletes them.
func (b *Backend) dropOnDurable(keys []string) func() {
	if len(keys) == 0 {
		return nil
	}
	for _, k := range keys {
		b.removing.add(k)
	}
	return func() {
		b.dropBlobs(keys)
		for _, k := range keys {
			b.removing.done(k)
		}
	}
}

// clear forgets every entry.
func (p *pendingSet) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m = nil
}

// dropBlobs deletes dereferenced blobs (deferred while readers hold them).
func (b *Backend) dropBlobs(keys []string) {
	var now []string
	for _, k := range keys {
		if b.blobs.deleteOrDefer(k) {
			now = append(now, k)
		}
	}
	b.deleteLater(now...)
}

// --- background deletes ---

// deleter deletes blobs in the background, in batches.
type deleter struct {
	mu   sync.Mutex
	keys []string
	wake chan struct{}
}

// deleteLater queues keys for deletion. Failures are logged; the orphan
// recovery pass removes leftovers.
func (b *Backend) deleteLater(keys ...string) {
	d := &b.del
	d.mu.Lock()
	n := len(d.keys)
	for _, k := range keys {
		if k != "" {
			d.keys = append(d.keys, k)
		}
	}
	added := len(d.keys) > n
	d.mu.Unlock()
	if !added {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// runDeleter is the deleter goroutine; it flushes the queue once more when
// the backend closes.
func (b *Backend) runDeleter() {
	defer b.wg.Done()
	d := &b.del
	for {
		select {
		case <-d.wake:
			b.flushDeletes()
		case <-b.ctx.Done():
			b.flushDeletes()
			return
		}
	}
}

func (b *Backend) flushDeletes() {
	d := &b.del
	d.mu.Lock()
	keys := d.keys
	d.keys = nil
	d.mu.Unlock()
	if len(keys) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := b.client().DeleteObjects(ctx, b.opts.Bucket, keys); err != nil {
		b.log.Warn("s3store: delete blobs (the recovery pass retries)", "backing", b.opts.Name, "count", len(keys), "err", err)
	}
}

// pendingDeletes returns the number of queued deletes (tests).
func (b *Backend) pendingDeletes() int {
	b.del.mu.Lock()
	defer b.del.mu.Unlock()
	return len(b.del.keys)
}

// --- uploads ---

// putBlob uploads r[0:size] as key. When the upload may have reached the
// service anyway, the blob is deleted again (best effort).
func (b *Backend) putBlob(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	_, _, err := b.client().Upload(ctx, b.opts.Bucket, key, r, size, s3client.UploadOptions{PartSize: b.conn.Load().partSize})
	if err != nil {
		if mayHaveLanded(err) {
			b.deleteLater(key)
		}
		return b.mapErr(ctx, "upload "+key, err)
	}
	return nil
}

// --- readers ---

// blobReader reads a blob of known size: a lazy, ranged-GET reader with its
// own position and a ReadAt, so the decompression layer can read it like a
// file. Close is idempotent; a tracked reader releases the blob.
type blobReader struct {
	b       *Backend
	key     string
	ctx     context.Context
	tracked bool
	missing func(err error) error

	mu   sync.Mutex
	r    *s3client.Reader
	size int64
	pos  int64

	once sync.Once
}

// newBlobReader returns a reader over the size bytes of blob key. A tracked
// reader must have been registered with b.blobs.acquire.
func (b *Backend) newBlobReader(ctx context.Context, key string, size int64, tracked bool, missing func(error) error) *blobReader {
	if ctx == nil {
		ctx = context.Background()
	}
	return &blobReader{
		b: b, key: key, ctx: ctx, tracked: tracked, missing: missing, size: size,
		r: b.client().NewReader(ctx, b.opts.Bucket, key, size, ""),
	}
}

func (r *blobReader) mapErr(err error) error {
	switch {
	case err == nil || errors.Is(err, io.EOF):
		return err
	case s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchBucket"):
		return r.missing(err)
	default:
		return r.b.mapErr(r.ctx, "read "+r.key, err)
	}
}

func (r *blobReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.r.Seek(r.pos, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	r.pos += int64(n)
	return n, r.mapErr(err)
}

// ReadAt implements io.ReaderAt.
func (r *blobReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("s3store: negative read offset")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if off >= r.size {
		return 0, io.EOF
	}
	want := p
	if rest := r.size - off; int64(len(want)) > rest {
		want = want[:rest]
	}
	if _, err := r.r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r.r, want)
	if err != nil {
		return n, r.mapErr(err)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *blobReader) Seek(off int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = r.pos + off
	case io.SeekEnd:
		abs = r.size + off
	default:
		return 0, errors.New("s3store: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("s3store: negative position")
	}
	r.pos = abs
	return abs, nil
}

func (r *blobReader) Close() error {
	r.once.Do(func() {
		_ = r.r.Close()
		if r.tracked {
			r.b.releaseBlob(r.key)
		}
	})
	return nil
}

// missingData is the missing function of object data readers.
func missingData(key string) func(error) error {
	return func(err error) error {
		return fmt.Errorf("%w: %s: %w", errBlobMissing, key, err)
	}
}

// missingPart is the missing function of part readers.
func missingPart(n int) func(error) error {
	return func(error) error { return fmt.Errorf("%w: part %d vanished", storage.ErrPartNotFound, n) }
}

// emptyReader is the reader of empty content.
type emptyReader struct{ *bytes.Reader }

func (emptyReader) Close() error { return nil }

// closeBoth closes a decompressing reader and the blob reader under it.
type closeBoth struct {
	io.ReadSeeker
	closers []io.Closer
}

func (c *closeBoth) Close() error {
	var err error
	for _, cl := range c.closers {
		if e := cl.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// --- server-side composition ---

// composeConcurrency is the number of UploadPartCopy requests a server-side
// composition runs in parallel.
const composeConcurrency = 8

// copyRange is one UploadPartCopy of a server-side composition.
type copyRange struct {
	src         string
	off, length int64
	part        int
}

// composeRanges splits the source blobs into UploadPartCopy ranges. It
// reports false when they cannot be composed server-side: a source other
// than the last is smaller than s3client.MinPartSize, or the result would
// need more than s3client.MaxParts parts.
func composeRanges(keys []string, sizes []int64, numbers []int) ([]copyRange, bool) {
	var out []copyRange
	for i, key := range keys {
		size := sizes[i]
		last := i == len(keys)-1
		if size == 0 && last {
			continue
		}
		if !last && size < s3client.MinPartSize {
			return nil, false
		}
		n := (size + s3client.MaxCopySize - 1) / s3client.MaxCopySize
		chunk := (size + n - 1) / n
		for off := int64(0); off < size; off += chunk {
			out = append(out, copyRange{src: key, off: off, length: min(chunk, size-off), part: numbers[i]})
		}
		if len(out) > s3client.MaxParts {
			return nil, false
		}
	}
	return out, len(out) > 0
}

// compose writes the concatenation of the source blobs to dst server-side:
// one source is copied, more are composed with UploadPartCopy.
func (b *Backend) compose(ctx context.Context, dst string, keys []string, sizes []int64, numbers []int) error {
	c := b.client()
	var err error
	if len(keys) == 1 {
		_, err = c.Copy(ctx, b.opts.Bucket, dst, keys[0], sizes[0], s3client.CopyOptions{})
		if s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchBucket") {
			return missingPart(numbers[0])(err)
		}
	} else if ranges, ok := composeRanges(keys, sizes, numbers); ok {
		err = b.composeServerSide(ctx, c, dst, ranges)
	} else {
		return errors.New("s3store: parts cannot be composed server-side")
	}
	if err != nil {
		if errors.Is(err, storage.ErrPartNotFound) {
			return err
		}
		if mayHaveLanded(err) {
			b.deleteLater(dst)
		}
		return b.mapErr(ctx, "compose "+dst, err)
	}
	return nil
}

func (b *Backend) composeServerSide(ctx context.Context, c *s3client.Client, dst string, ranges []copyRange) error {
	upID, err := c.CreateMultipartUpload(ctx, b.opts.Bucket, dst, nil)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	parts := make([]s3client.CompletedPart, len(ranges))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, composeConcurrency)
	for i, cr := range ranges {
		select {
		case sem <- struct{}{}:
		case <-cctx.Done():
		}
		if cctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			etag, err := c.UploadPartCopy(cctx, b.opts.Bucket, dst, upID, i+1, cr.src, cr.off, cr.length, "")
			if err != nil {
				if s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchUpload") && !s3client.IsCode(err, "NoSuchBucket") {
					err = missingPart(cr.part)(err)
				}
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				cancel()
				return
			}
			parts[i] = s3client.CompletedPart{Number: i + 1, ETag: etag}
		}()
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = cctx.Err()
	}
	if firstErr == nil {
		_, firstErr = c.CompleteMultipartUpload(ctx, b.opts.Bucket, dst, upID, parts, s3client.CompleteOptions{})
	}
	if firstErr != nil {
		actx, acancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer acancel()
		_ = c.AbortMultipartUpload(actx, b.opts.Bucket, dst, upID)
		return firstErr
	}
	return nil
}
