package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

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

// spoolMemory is how much of an object or part being written is buffered
// in memory before the spool spills to a file in tmp/.
const spoolMemory = 8 << 20

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

// newSpool returns a spool for data headed to a remote.
func (b *Backend) newSpool() *s3client.Spool { return s3client.NewSpool(b.tmpDir, spoolMemory) }

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

// blobReader is the reader OpenObject returns for remote data: a lazy,
// ranged-GET reader whose Close is idempotent and releases the blob.
type blobReader struct {
	b   *Backend
	rem *remote
	key string
	ctx context.Context
	r   *s3client.Reader

	once sync.Once
}

func (r *blobReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		if s3client.IsNotFound(err) {
			err = fmt.Errorf("%w: remote blob %s of backing %q is missing: %w", errCorrupt, r.key, r.rem.name, err)
		} else {
			err = r.rem.mapErr(r.ctx, "read "+r.key, err)
		}
	}
	return n, err
}

func (r *blobReader) Seek(off int64, whence int) (int64, error) { return r.r.Seek(off, whence) }

// Close closes the reader; subsequent calls return nil.
func (r *blobReader) Close() error {
	r.once.Do(func() {
		_ = r.r.Close()
		r.b.releaseBlob(r.rem, r.key)
	})
	return nil
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

// composeRemote writes the concatenation of the listed part blobs to dst.
// A single part is copied, parts that satisfy the service's multipart
// limits are composed server-side with UploadPartCopy, anything else is
// streamed through a spool. A part blob that vanished yields
// errPartsChanged.
func (b *Backend) composeRemote(ctx context.Context, r *remote, dst, uploadID string, files []string, sizes []int64, numbers []int) error {
	keys := make([]string, len(files))
	for i, f := range files {
		keys[i] = r.partKey(uploadID, f)
	}
	var err error
	if len(keys) == 1 {
		_, err = r.client.Copy(ctx, r.bucket, dst, keys[0], sizes[0], s3client.CopyOptions{})
		if s3client.IsNotFound(err) {
			return fmt.Errorf("%w: part %d vanished", errPartsChanged, numbers[0])
		}
	} else if ranges, ok := composeRanges(keys, sizes, numbers); ok {
		err = b.composeServerSide(ctx, r, dst, ranges)
	} else {
		return b.composeStreaming(ctx, r, dst, keys, sizes, numbers)
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

// composeStreaming downloads the part blobs into a spool, verifying their
// sizes, and uploads the result as dst.
func (b *Backend) composeStreaming(ctx context.Context, r *remote, dst string, keys []string, sizes []int64, numbers []int) error {
	sp := b.newSpool()
	defer sp.Close()
	for i, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, _, err := r.client.GetObject(ctx, r.bucket, key, s3client.GetOptions{})
		if err != nil {
			if s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchBucket") {
				return fmt.Errorf("%w: part %d vanished", errPartsChanged, numbers[i])
			}
			return r.mapErr(ctx, "read part blob "+key, err)
		}
		n, err := io.Copy(sp, io.LimitReader(body, sizes[i]+1))
		_ = body.Close()
		if err != nil {
			return r.mapErr(ctx, fmt.Sprintf("copy part %d", numbers[i]), err)
		}
		if n != sizes[i] {
			return fmt.Errorf("%w: part %d blob has %d bytes, expected %d", errCorrupt, numbers[i], n, sizes[i])
		}
	}
	if err := sp.Finish(); err != nil {
		return err
	}
	return b.putBlob(ctx, r, dst, sp, sp.Size())
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
