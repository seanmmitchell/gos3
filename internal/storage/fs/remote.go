package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// RemoteStore is a remote S3 bucket holding the object data of buckets
// whose Backing names it (blob mode). The bucket records, versions and all
// other metadata stay in meta.db; only object data and multipart part data
// live in the remote bucket, as opaque blobs under Prefix (see the package
// documentation for the layout). The remote bucket and prefix must be used
// by this backend only: the recovery pass deletes unreferenced blobs there.
type RemoteStore struct {
	Client   *s3client.Client
	Bucket   string // remote bucket (must exist)
	Prefix   string // prefix of every remote key, e.g. "gos3/" ("" allowed)
	PartSize int64  // multipart part size for uploads, 0 = s3client.DefaultPartSize
}

// remoteCleanupTimeout bounds remote deletes that run after a metadata
// transaction committed; they must not depend on the request's context.
const remoteCleanupTimeout = 2 * time.Minute

// composeConcurrency is the number of UploadPartCopy requests a server-side
// CompleteMultipartUpload runs in parallel.
const composeConcurrency = 8

// remote is a configured blob backing.
type remote struct {
	name   string
	client *s3client.Client
	bucket string
	prefix string
	part   int64
}

func newRemotes(in map[string]RemoteStore) (map[string]*remote, error) {
	out := make(map[string]*remote, len(in))
	for name, rs := range in {
		if name == "" {
			return nil, errors.New("fs: remote backing with an empty name")
		}
		if rs.Client == nil || rs.Bucket == "" {
			return nil, fmt.Errorf("fs: remote backing %q needs a client and a bucket", name)
		}
		if rs.PartSize < 0 {
			return nil, fmt.Errorf("fs: remote backing %q: negative part size", name)
		}
		out[name] = &remote{name: name, client: rs.Client, bucket: rs.Bucket, prefix: rs.Prefix, part: rs.PartSize}
	}
	return out, nil
}

// sortedRemotes returns the configured remotes ordered by name.
func (b *Backend) sortedRemotes() []*remote {
	out := make([]*remote, 0, len(b.remotes))
	for _, r := range b.remotes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// remoteFor returns the remote holding the data of a bucket with the given
// backing: nil for local storage, an error wrapping
// storage.ErrBackingUnavailable for a backing that is not a configured blob
// backing (e.g. a gateway backing, or one removed from the configuration).
func (b *Backend) remoteFor(backing string) (*remote, error) {
	if backing == "" {
		return nil, nil
	}
	if r := b.remotes[backing]; r != nil {
		return r, nil
	}
	return nil, fmt.Errorf("%w: backing %q is not a configured blob backing of this storage", storage.ErrBackingUnavailable, backing)
}

// --- keys ---

func (r *remote) dataPrefix() string   { return r.prefix + "data/" }
func (r *remote) uploadPrefix() string { return r.prefix + "uploads/" }

func (r *remote) dataKey(bucketID, dataID string) string {
	return r.dataPrefix() + bucketID + "/" + dataID
}

func (r *remote) uploadKeyPrefix(uploadID string) string {
	return r.uploadPrefix() + uploadID + "/"
}

func (r *remote) partKey(uploadID, file string) string {
	return r.uploadKeyPrefix(uploadID) + file
}

// blobID identifies a remote blob in the pending set and the reader
// registry. It names the service and bucket rather than the backing, so two
// backings configured on the same remote bucket still share the entries.
func (r *remote) blobID(key string) string {
	return "remote\x00" + r.client.Endpoint() + "\x00" + r.bucket + "\x00" + key
}

// --- errors ---

// mapErr turns a remote failure into the backend's error vocabulary:
// unreachable or failing services wrap storage.ErrBackingUnavailable (the
// S3 layer answers 503); the context's own error is returned as such.
func (r *remote) mapErr(ctx context.Context, what string, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if s3client.IsUnavailable(err) {
		return fmt.Errorf("%w: backing %q: %s: %w", storage.ErrBackingUnavailable, r.name, what, err)
	}
	return fmt.Errorf("fs: backing %q: %s: %w", r.name, what, err)
}

// cleanupCtx returns a context for remote cleanups that must happen even
// when the request that triggered them is gone.
func cleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), remoteCleanupTimeout)
}

// --- uploads ---

// bucketPlacement returns the ID of bucket and the remote holding its data
// (nil: local files). A bucket whose backing is not a configured blob
// backing yields an error wrapping storage.ErrBackingUnavailable.
func (b *Backend) bucketPlacement(ctx context.Context, bucket string) (string, *remote, error) {
	if err := b.begin(ctx); err != nil {
		return "", nil, err
	}
	var id, backing string
	err := b.view(func(tx *bbolt.Tx) error {
		br, _, err := loadBucket(tx, bucket)
		if err == nil {
			id, backing = br.ID, br.Backing
		}
		return err
	})
	if err != nil {
		return "", nil, err
	}
	rem, err := b.remoteFor(backing)
	if err != nil {
		return "", nil, err
	}
	return id, rem, nil
}

// uploadTemp uploads the finished temporary file t as the blob key and
// removes t. The blob's ID must already be registered as pending.
func (b *Backend) uploadTemp(ctx context.Context, r *remote, key string, t *tmpFile) error {
	defer t.remove()
	if ctx == nil {
		ctx = context.Background()
	}
	if t.path == "" {
		// Nothing was written (an empty part): upload an empty blob.
		return b.putBlob(ctx, r, key, bytes.NewReader(nil), 0)
	}
	f, err := os.Open(t.path)
	if err != nil {
		return fmt.Errorf("fs: open temporary file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("fs: stat temporary file: %w", err)
	}
	return b.putBlob(ctx, r, key, f, st.Size())
}

// putBlob uploads size bytes of src as key. When the upload may have
// reached the service anyway (a transport failure rather than an error
// answer) the blob is removed again, best effort.
func (b *Backend) putBlob(ctx context.Context, r *remote, key string, src io.ReaderAt, size int64) error {
	_, _, err := r.client.Upload(ctx, r.bucket, key, src, size, s3client.UploadOptions{PartSize: r.part})
	if err != nil {
		if mayHaveLanded(err) {
			b.removeBlob(r, key)
		}
		return r.mapErr(ctx, "upload "+key, err)
	}
	return nil
}

// mayHaveLanded reports whether a failed write request may nevertheless
// have been performed: anything but an error answer of the service.
func mayHaveLanded(err error) bool {
	var e *s3client.Error
	return !errors.As(err, &e) || e.StatusCode == 0
}

// removeBlob deletes a blob that no reader can hold (it was never
// referenced by committed metadata, or it is a part blob). Failures are
// logged; the recovery pass removes leftovers.
func (b *Backend) removeBlob(r *remote, key string) {
	ctx, cancel := cleanupCtx()
	defer cancel()
	if err := r.client.DeleteObject(ctx, r.bucket, key); err != nil {
		b.log.Warn("fs: delete remote blob", "backing", r.name, "key", key, "err", err)
	}
}

// dropBlob deletes a data blob whose version was dereferenced by a
// committed transaction, or defers the delete until the last open reader
// of the blob is closed.
func (b *Backend) dropBlob(r *remote, key string) {
	if b.blobs.deleteOrDefer(r.blobID(key)) {
		b.removeBlob(r, key)
	}
}

// dropData removes the data of versions dereferenced by a committed
// transaction of a bucket with the given backing.
func (b *Backend) dropData(backing, bucketID string, ids []string) {
	if len(ids) == 0 {
		return
	}
	if backing == "" {
		b.unlinkData(bucketID, ids)
		return
	}
	r := b.remotes[backing]
	if r == nil {
		b.log.Warn("fs: backing of deleted object data is not configured; leaving its blobs in place",
			"backing", backing, "bucket_id", bucketID, "count", len(ids))
		return
	}
	for _, id := range ids {
		if validDataID(id) {
			b.dropBlob(r, r.dataKey(bucketID, id))
		}
	}
}

// removeUploadData removes the part data of an upload whose records are
// gone.
func (b *Backend) removeUploadData(backing, uploadID string) {
	if backing == "" {
		b.removeUploadDir(uploadID)
		return
	}
	r := b.remotes[backing]
	if r == nil || !validUploadID(uploadID) {
		return
	}
	b.purgeRemotePrefix(r, r.uploadKeyPrefix(uploadID))
}

// purgeRemotePrefix deletes every blob below prefix that is neither pending
// nor held by an open reader (those are deleted by their owners).
func (b *Backend) purgeRemotePrefix(r *remote, prefix string) {
	ctx, cancel := cleanupCtx()
	defer cancel()
	var keys []string
	err := r.client.Walk(ctx, r.bucket, prefix, "", func(o s3client.ObjectAttrs) error {
		keys = append(keys, o.Key)
		return nil
	})
	if err != nil {
		b.log.Warn("fs: list remote blobs to delete", "backing", r.name, "prefix", prefix, "err", err)
	}
	for _, k := range keys {
		id := r.blobID(k)
		if b.pending.has(id) {
			continue
		}
		if b.blobs.deleteOrDefer(id) {
			if err := r.client.DeleteObject(ctx, r.bucket, k); err != nil {
				b.log.Warn("fs: delete remote blob", "backing", r.name, "key", k, "err", err)
			}
		}
	}
}

// --- open readers ---

// blobTracker counts open readers per remote data blob and defers deleting
// a blob that is dereferenced while readers are open until the last one is
// closed, giving remote blobs the unlink semantics of local data files.
type blobTracker struct {
	mu   sync.Mutex
	refs map[string]*blobRef
}

type blobRef struct {
	readers int
	// deleteOnClose is set when the blob was dereferenced while open.
	deleteOnClose bool
}

// acquire registers an open reader of blob id.
func (t *blobTracker) acquire(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.refs == nil {
		t.refs = make(map[string]*blobRef)
	}
	ref := t.refs[id]
	if ref == nil {
		ref = &blobRef{}
		t.refs[id] = ref
	}
	ref.readers++
}

// release unregisters a reader and reports whether the blob must be
// deleted now (it was dereferenced and this was its last reader).
func (t *blobTracker) release(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ref := t.refs[id]
	if ref == nil {
		return false
	}
	ref.readers--
	if ref.readers > 0 {
		return false
	}
	delete(t.refs, id)
	return ref.deleteOnClose
}

// deleteOrDefer reports whether a dereferenced blob may be deleted now;
// if readers hold it, it marks it for deletion by the last one instead.
func (t *blobTracker) deleteOrDefer(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ref := t.refs[id]; ref != nil && ref.readers > 0 {
		ref.deleteOnClose = true
		return false
	}
	return true
}

// held reports whether blob id has open readers.
func (t *blobTracker) held(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ref := t.refs[id]
	return ref != nil && ref.readers > 0
}

// releaseBlob unregisters a reader of key and performs a deferred delete.
func (b *Backend) releaseBlob(r *remote, key string) {
	if b.blobs.release(r.blobID(key)) {
		b.removeBlob(r, key)
	}
}

// blobReader reads a remote blob of known size: a lazy, ranged-GET reader
// (s3client.Reader) with its own Read/Seek position and a ReadAt, so the
// decryption and decompression layers can read it like a data file.
// Sequential reads and ReadAts share one GET stream; a jump reopens it at
// the new offset. Close is idempotent; for a tracked reader (OpenObject)
// it releases the blob (see blobTracker).
type blobReader struct {
	b       *Backend
	rem     *remote
	key     string
	ctx     context.Context
	tracked bool
	// missing maps a 404 of the blob (errCorrupt for object data,
	// errPartsChanged for parts).
	missing func(err error) error

	mu   sync.Mutex
	r    *s3client.Reader
	size int64
	pos  int64

	once sync.Once
}

// newBlobReader returns a reader over the size bytes of blob key. tracked
// readers must have been registered with b.blobs.acquire.
func (b *Backend) newBlobReader(ctx context.Context, rem *remote, key string, size int64, tracked bool, missing func(error) error) *blobReader {
	if ctx == nil {
		ctx = context.Background()
	}
	return &blobReader{
		b: b, rem: rem, key: key, ctx: ctx, tracked: tracked, missing: missing, size: size,
		r: rem.client.NewReader(ctx, rem.bucket, key, size, ""),
	}
}

// mapErr translates a read error of the remote.
func (r *blobReader) mapErr(err error) error {
	switch {
	case err == nil || errors.Is(err, io.EOF):
		return err
	case s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchBucket"):
		return r.missing(err)
	default:
		return r.rem.mapErr(r.ctx, "read "+r.key, err)
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

// ReadAt implements io.ReaderAt (serialized; reads at the offset where the
// previous one ended keep using the open GET stream).
func (r *blobReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("fs: negative read offset")
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
		return 0, errors.New("fs: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("fs: negative position")
	}
	r.pos = abs
	return abs, nil
}

// Close closes the reader; subsequent calls return nil.
func (r *blobReader) Close() error {
	r.once.Do(func() {
		_ = r.r.Close()
		if r.tracked {
			r.b.releaseBlob(r.rem, r.key)
		}
	})
	return nil
}

// corruptBlob is the missing function of object data readers.
func (r *remote) corruptBlob(key string) func(error) error {
	return func(err error) error {
		return fmt.Errorf("%w: remote blob %s of backing %q is missing: %w", errCorrupt, key, r.name, err)
	}
}

// vanishedPart is the missing function of part readers.
func vanishedPart(n int) func(error) error {
	return func(error) error { return fmt.Errorf("%w: part %d vanished", errPartsChanged, n) }
}

// --- multipart completion ---

// copyRange is one UploadPartCopy of a server-side composition.
type copyRange struct {
	src         string
	off, length int64
	part        int // source part number (for errors)
}

// composeRanges splits the listed part blobs into UploadPartCopy ranges.
// It reports false when the parts cannot be composed server-side: a part
// other than the last is smaller than s3client.MinPartSize, or the result
// would need more than s3client.MaxParts parts.
func composeRanges(keys []string, sizes []int64, numbers []int) ([]copyRange, bool) {
	var out []copyRange
	for i, key := range keys {
		size := sizes[i]
		last := i == len(keys)-1
		if size == 0 && last {
			continue // contributes nothing
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

// composable reports whether parts of the given stored sizes can be
// concatenated server-side: a single part, or parts that satisfy the
// service's multipart limits (see composeRanges).
func composable(sizes []int64) bool {
	if len(sizes) == 1 {
		return true
	}
	keys := make([]string, len(sizes))
	nums := make([]int, len(sizes))
	_, ok := composeRanges(keys, sizes, nums)
	return ok
}

// composeRemote writes the concatenation of the listed part blobs (whose
// stored bytes are the content as is) to dst server-side: a single part is
// copied, more parts are composed with UploadPartCopy. The caller checks
// composable first. A part blob that vanished yields errPartsChanged.
func (b *Backend) composeRemote(ctx context.Context, r *remote, dst, uploadID string, files []string, sizes []int64, numbers []int) error {
	keys := make([]string, len(files))
	for i, f := range files {
		keys[i] = r.partKey(uploadID, f)
	}
	var err error
	if len(keys) == 1 {
		_, err = r.client.Copy(ctx, r.bucket, dst, keys[0], sizes[0], s3client.CopyOptions{})
		if s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchBucket") {
			return fmt.Errorf("%w: part %d vanished", errPartsChanged, numbers[0])
		}
	} else if ranges, ok := composeRanges(keys, sizes, numbers); ok {
		err = b.composeServerSide(ctx, r, dst, ranges)
	} else {
		return errors.New("fs: parts cannot be composed server-side")
	}
	if err != nil {
		if errors.Is(err, errPartsChanged) || errors.Is(err, errCorrupt) {
			return err
		}
		if mayHaveLanded(err) {
			b.removeBlob(r, dst)
		}
		return r.mapErr(ctx, "compose "+dst, err)
	}
	return nil
}

// composeServerSide runs a remote multipart upload of dst whose parts are
// copied from ranges.
func (b *Backend) composeServerSide(ctx context.Context, r *remote, dst string, ranges []copyRange) error {
	upID, err := r.client.CreateMultipartUpload(ctx, r.bucket, dst, nil)
	if err != nil {
		return err
	}
	abort := func() {
		actx, cancel := cleanupCtx()
		defer cancel()
		_ = r.client.AbortMultipartUpload(actx, r.bucket, dst, upID)
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
		go func(i int, cr copyRange) {
			defer wg.Done()
			defer func() { <-sem }()
			etag, err := r.client.UploadPartCopy(cctx, r.bucket, dst, upID, i+1, cr.src, cr.off, cr.length, "")
			if err != nil {
				switch {
				case s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchUpload") && !s3client.IsCode(err, "NoSuchBucket"):
					err = fmt.Errorf("%w: part %d vanished", errPartsChanged, cr.part)
				case s3client.IsCode(err, "InvalidRange"):
					err = fmt.Errorf("%w: part %d blob is shorter than recorded: %w", errCorrupt, cr.part, err)
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
		}(i, cr)
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = cctx.Err()
	}
	if firstErr == nil {
		_, firstErr = r.client.CompleteMultipartUpload(ctx, r.bucket, dst, upID, parts, s3client.CompleteOptions{})
	}
	if firstErr != nil {
		abort()
		return firstErr
	}
	return nil
}

// --- startup check ---

// remoteHoldsBlobs returns a description of the first configured remote
// that holds blobs below its data/ or uploads/ prefix ("" if none). A
// remote that cannot be reached is logged and skipped.
func (b *Backend) remoteHoldsBlobs() string {
	for _, r := range b.sortedRemotes() {
		for _, p := range []string{r.dataPrefix(), r.uploadPrefix()} {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			out, err := r.client.ListObjectsV2(ctx, r.bucket, s3client.ListInput{Prefix: p, MaxKeys: 1})
			cancel()
			if err != nil {
				b.log.Warn("fs: cannot check remote backing for stored blobs", "backing", r.name, "bucket", r.bucket, "prefix", p, "err", err)
				break
			}
			if len(out.Objects) > 0 {
				return fmt.Sprintf("backing %q (bucket %q, prefix %q)", r.name, r.bucket, p)
			}
		}
	}
	return ""
}

// trimKey returns key without prefix p and whether it had it.
func trimKey(key, p string) (string, bool) {
	if !strings.HasPrefix(key, p) {
		return "", false
	}
	return key[len(p):], true
}
